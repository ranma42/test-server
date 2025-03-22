package record

import (
	"fmt"
	"net/http"

	"github.com/google/test-server/internal/config"
	"github.com/google/test-server/internal/store"
)

type RecordingHTTPSProxy struct {
	prevRequestSHA store.SHA256Sum
	config         *config.EndpointConfig
}

func NewRecordingHTTPSProxy(cfg *config.EndpointConfig) *RecordingHTTPSProxy {
	return &RecordingHTTPSProxy{
		prevRequestSHA: store.HeadSHA(),
		config:         cfg,
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
	recordedRequest, err := store.NewRecordedRequest(req, r.prevRequestSHA)
	if err != nil {
		panic(err)
	}
	s := recordedRequest.Serialize()

	fmt.Printf("%v", s)
	// TODO: Implement the request handling logic here
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Hello, world!"))
}
