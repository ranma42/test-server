package record

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/test-server/internal/config"
	"github.com/google/test-server/internal/store"
)

type RecordingHTTPSProxy struct {
	prevRequestSHA store.SHA256Sum
	config         *config.EndpointConfig
	recordingDir   string
}

func NewRecordingHTTPSProxy(cfg *config.EndpointConfig, recordingDir string) *RecordingHTTPSProxy {
	return &RecordingHTTPSProxy{
		prevRequestSHA: store.HeadSHA(),
		config:         cfg,
		recordingDir:   recordingDir,
	}
}

func (r *RecordingHTTPSProxy) Start() error {
	addr := fmt.Sprintf(":%d", r.config.SourcePort)
	server := &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(r.handleRequest),
	}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
	return nil
}

func (r *RecordingHTTPSProxy) handleRequest(w http.ResponseWriter, req *http.Request) {
	reqHash, err := r.recordRequest(req)
	if err != nil {
		fmt.Printf("Error recording request: %v\n", err)
		http.Error(w, fmt.Sprintf("Error recording request: %v", err), http.StatusInternalServerError)
		return
	}

	resp, respBody, err := r.proxyRequest(w, req)
	if err != nil {
		fmt.Printf("Error proxying request: %v\n", err)
		http.Error(w, fmt.Sprintf("Error proxying request: %v", err), http.StatusInternalServerError)
		return
	}

	err = r.recordResponse(resp, reqHash, respBody)

	if err != nil {
		fmt.Printf("Error recording response: %v\n", err)
		http.Error(w, fmt.Sprintf("Error recording response: %v", err), http.StatusInternalServerError)
		return
	}
}

func (r *RecordingHTTPSProxy) recordRequest(req *http.Request) (string, error) {
	recordedRequest, err := store.NewRecordedRequest(req, r.prevRequestSHA, *r.config)
	if err != nil {
		return "", err
	}

	recordedRequest.RedactHeaders(r.config.RedactRequestHeaders)

	reqHash, err := recordedRequest.ComputeSum()
	if err != nil {
		return "", err
	}
	reqHashHex := hex.EncodeToString(reqHash[:])

	recordPath := filepath.Join(r.recordingDir, reqHashHex+".req")
	err = os.WriteFile(recordPath, []byte(recordedRequest.Serialize()), 0644)
	if err != nil {
		return "", err
	}
	return reqHashHex, nil
}

func (r *RecordingHTTPSProxy) proxyRequest(w http.ResponseWriter, req *http.Request) (*http.Response, []byte, error) {
	url := fmt.Sprintf("https://%s:%d%s", r.config.TargetHost, r.config.TargetPort, req.URL.Path)
	if req.URL.RawQuery != "" {
		url += "?" + req.URL.RawQuery
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, nil, err
	}
	req.Body.Close()

	proxyReq, err := http.NewRequest(req.Method, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, err
	}

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		return nil, nil, err
	}

	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	w.WriteHeader(resp.StatusCode)

	respBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	w.Write(respBodyBytes) // Send original (compressed) body to client
	return resp, respBodyBytes, nil
}

func (r *RecordingHTTPSProxy) recordResponse(resp *http.Response, reqHash string, body []byte) error {
	recordedResponse, err := store.NewRecordedResponse(resp, body)
	if err != nil {
		return err
	}

	recordPath := filepath.Join(r.recordingDir, reqHash+".resp")
	err = os.WriteFile(recordPath, []byte(recordedResponse.Serialize()), 0644)
	if err != nil {
		return err
	}

	return nil
}
