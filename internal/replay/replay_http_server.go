package replay

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/test-server/internal/config"
	"github.com/google/test-server/internal/store"
)

type ReplayHTTPServer struct {
	prevRequestSHA string
	config         *config.EndpointConfig
	recordingDir   string
}

func NewReplayHTTPServer(cfg *config.EndpointConfig, recordingDir string) *ReplayHTTPServer {
	return &ReplayHTTPServer{
		prevRequestSHA: store.HeadSHA,
		config:         cfg,
		recordingDir:   recordingDir,
	}
}

func (r *ReplayHTTPServer) Start() error {
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

func (r *ReplayHTTPServer) handleRequest(w http.ResponseWriter, req *http.Request) {
	fmt.Printf("Replaying request: %s %s\n", req.Method, req.URL.String())
	reqHash, err := r.computeRequestHash(req)
	if err != nil {
		fmt.Printf("Error computing request sum: %v\n", err)
		http.Error(w, fmt.Sprintf("Error computing request sum: %v", err), http.StatusInternalServerError)
		return
	}

	resp, err := r.loadResponse(reqHash)
	if err != nil {
		fmt.Printf("Error loading response: %v\n", err)
		http.Error(w, fmt.Sprintf("Error loading response: %v", err), http.StatusInternalServerError)
		return
	}

	err = r.writedResponse(w, resp)
	if err != nil {
		fmt.Printf("Error writing response: %v\n", err)
		panic(err)
	}
}

func (r *ReplayHTTPServer) computeRequestHash(req *http.Request) (string, error) {
	recordedRequest, err := store.NewRecordedRequest(req, r.prevRequestSHA, *r.config)
	if err != nil {
		return "", err
	}

	recordedRequest.RedactHeaders(r.config.RedactRequestHeaders)

	reqHash, err := recordedRequest.ComputeSum()
	if err != nil {
		return "", err
	}

	return reqHash, nil
}

func (r *ReplayHTTPServer) loadResponse(sha string) (*store.RecordedResponse, error) {
	responseFile := filepath.Join(r.recordingDir, sha+".resp")
	responseData, err := os.ReadFile(responseFile)
	if err != nil {
		return nil, err
	}
	return store.DeserializeResponse(responseData)
}

func (r *ReplayHTTPServer) writedResponse(w http.ResponseWriter, resp *store.RecordedResponse) error {
	w.WriteHeader(resp.StatusCode)

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	_, err := w.Write(resp.Body)
	return err
}
