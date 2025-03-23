/*
Copyright 2025 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package replay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/test-server/internal/config"
)

// Replay serves recorded responses for HTTP requests
func Replay(cfg *config.TestServerConfig, recordingDir string) error {
	// Validate recording directory exists
	if _, err := os.Stat(recordingDir); os.IsNotExist(err) {
		return fmt.Errorf("recording directory does not exist: %s", recordingDir)
	}

	fmt.Printf("Replaying from directory: %s\n", recordingDir)

	// Start a server for each endpoint
	errChan := make(chan error, len(cfg.Endpoints))

	for _, endpoint := range cfg.Endpoints {
		go func(ep config.EndpointConfig) {
			server := NewReplayHTTPServer(&endpoint, recordingDir)
			err := server.Start()
			if err != nil {
				errChan <- fmt.Errorf("replay error for %s:%d: %w",
					ep.TargetHost, ep.TargetPort, err)
			}
		}(endpoint)
	}

	// Return the first error encountered, if any
	select {
	case err := <-errChan:
		return err
	default:
		// Block forever (or until interrupted)
		select {}
	}
}

// replayHTTPToHTTPS handles replaying HTTP to HTTPS recordings
func replayHTTPToHTTPS(endpoint config.EndpointConfig, recordingDir string) error {
	// Create an HTTP server
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", endpoint.SourcePort),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleReplay(w, r, endpoint, recordingDir)
		}),
	}

	fmt.Printf("HTTP replay server listening on :%d, serving recordings for https://%s:%d\n",
		endpoint.SourcePort, endpoint.TargetHost, endpoint.TargetPort)

	return server.ListenAndServe()
}

func handleReplay(w http.ResponseWriter, r *http.Request, endpoint config.EndpointConfig, recordingDir string) {
	// Log the incoming request
	fmt.Printf("[REPLAY] Received request: %s %s\n", r.Method, r.URL.Path)

	// First read the entire body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		fmt.Printf("[REPLAY] Error reading request body: %v\n", err)
		return
	}
	r.Body.Close()

	// Create a copy of the request for hashing with the body restored
	reqCopy := r.Clone(r.Context())
	reqCopy.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Remove any redacted headers before hashing
	for _, header := range endpoint.RedactRequestHeaders {
		reqCopy.Header.Del(header)
	}

	// Dump the request for hashing
	reqDump, err := httputil.DumpRequest(reqCopy, true)
	if err != nil {
		http.Error(w, "Failed to process request", http.StatusInternalServerError)
		fmt.Printf("[REPLAY] Error dumping request: %v\n", err)
		return
	}

	// Calculate SHA256 hash of the request (with redacted headers removed)
	hasher := sha256.New()
	hasher.Write(reqDump)
	requestHash := hex.EncodeToString(hasher.Sum(nil))

	// Look for the recorded response
	responseFile := filepath.Join(recordingDir, requestHash)
	responseData, err := os.ReadFile(responseFile)
	if err != nil {
		http.Error(w, "No recording found for this request", http.StatusNotFound)
		fmt.Printf("[REPLAY] No recording found for hash: %s\n", requestHash)
		return
	}

	// Check if we have a compressed version of the response
	compressedFile := responseFile + ".gz"
	hasCompressedVersion := false
	compressedData, err := os.ReadFile(compressedFile)
	if err == nil {
		hasCompressedVersion = true
		fmt.Printf("[REPLAY] Found compressed version of response\n")
	}

	resp, err := parseResponseData(responseData, r, compressedData, hasCompressedVersion)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeResponseToClient(w, resp, responseData, compressedData, hasCompressedVersion)

	fmt.Printf("[REPLAY] Successfully replayed response for: %s %s\n", r.Method, r.URL.Path)
}

func parseResponseData(responseData []byte, r *http.Request, compressedData []byte, hasCompressedVersion bool) (*http.Response, error) {
	// Try to parse the response - if it fails, we'll handle it differently
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(responseData)), r)
	if err != nil {
		fmt.Printf("[REPLAY] Error parsing recorded response: %v\n", err)
		fmt.Printf("[REPLAY] Attempting alternative response handling...\n")

		// Try to extract headers and body manually
		parts := bytes.SplitN(responseData, []byte("\r\n\r\n"), 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("Failed to parse recorded response")
		}

		// Parse status line and headers
		headerLines := bytes.Split(parts[0], []byte("\r\n"))
		if len(headerLines) < 1 {
			return nil, fmt.Errorf("Invalid response format")
		}

		// Parse status line (e.g., "HTTP/1.1 200 OK")
		statusLine := string(headerLines[0])
		statusParts := strings.SplitN(statusLine, " ", 3)
		if len(statusParts) < 2 {
			return nil, fmt.Errorf("Invalid status line")
		}

		statusCode := 200 // Default
		if code, err := strconv.Atoi(statusParts[1]); err == nil {
			statusCode = code
		}

		// Create a new Response object
		resp = &http.Response{
			Status:     statusLine,
			StatusCode: statusCode,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(parts[1])), // Set the body here
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
		}

		// Check if response should be gzipped
		isGzipped := false
		for i := 1; i < len(headerLines); i++ {
			headerLine := string(headerLines[i])
			if strings.Contains(strings.ToLower(headerLine), "content-encoding: gzip") {
				isGzipped = true
				break
			}
		}

		// Parse headers
		for i := 1; i < len(headerLines); i++ {
			headerLine := string(headerLines[i])
			headerParts := strings.SplitN(headerLine, ":", 2)
			if len(headerParts) == 2 {
				key := strings.TrimSpace(headerParts[0])
				value := strings.TrimSpace(headerParts[1])
				resp.Header.Add(key, value)
			}
		}

		// Handle gzipped content
		if isGzipped && hasCompressedVersion {
			// Use the original compressed data if available
			fmt.Printf("[REPLAY] Using original compressed response (%d bytes)\n", len(compressedData))
			resp.Body = io.NopCloser(bytes.NewReader(compressedData))
		} else if isGzipped {
			// Decompress the body if needed and no original compressed version exists
			gzipReader, err := gzip.NewReader(bytes.NewReader(parts[1]))
			if err != nil {
				fmt.Printf("[REPLAY] Error creating gzip reader: %v\n", err)
				return nil, fmt.Errorf("Error creating gzip reader: %v", err)
			}
			resp.Body = gzipReader
		}

		fmt.Printf("[REPLAY] Successfully replayed response using alternative method\n")
		return resp, nil
	}
	return resp, nil
}

func writeResponseToClient(w http.ResponseWriter, resp *http.Response, responseData []byte, compressedData []byte, hasCompressedVersion bool) {
	defer resp.Body.Close()

	// Check if response should be gzipped based on Content-Encoding header
	isGzipped := false
	for _, value := range resp.Header["Content-Encoding"] {
		if strings.Contains(strings.ToLower(value), "gzip") {
			isGzipped = true
			break
		}
	}

	// Copy headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Set status code
	w.WriteHeader(resp.StatusCode)

	// Write body, compressing if needed
	if isGzipped {
		// Use the original compressed data if available
		fmt.Printf("[REPLAY] Using original compressed response (%d bytes)\n", len(compressedData))
		w.Write(compressedData)
	} else {
		parts := bytes.SplitN(responseData, []byte("\r\n\r\n"), 2)
		if len(parts) != 2 {
			// TODO: we need better error handling
			panic("can't split response body")
		}
		fmt.Printf("[REPLAY] Using original uncompressed response (%d bytes)\n", len(responseData))
		fmt.Printf("[REPLAY] %v\n", string(parts[1]))
		w.Write(parts[1])
	}
}
