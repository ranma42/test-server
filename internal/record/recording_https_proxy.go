package record

import (
	"encoding/hex"
	"fmt"
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
	recordedRequest, err := store.NewRecordedRequest(req, r.prevRequestSHA, *r.config)
	if err != nil {
		panic(err)
	}

	reqHash, err := recordedRequest.ComputeSum()
	if err != nil {
		panic(err)
	}
	reqHashHex := hex.EncodeToString(reqHash[:])
	s := recordedRequest.Serialize()

	recordPath := filepath.Join(r.recordingDir, reqHashHex+".req")
	err = os.WriteFile(recordPath, []byte(recordedRequest.Serialize()), 0644)
	if err != nil {
		fmt.Printf("Error saving request to file: %v\n", err)
	}

	fmt.Printf("%v", s)
	// TODO: Implement the request handling logic here
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Hello, world!"))
}
