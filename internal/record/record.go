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

package record

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/test-server/internal/config"
)

func Record(cfg *config.TestServerConfig, recordingDir string) error {
	// Create recording directory if it doesn't exist
	if err := os.MkdirAll(recordingDir, 0755); err != nil {
		return fmt.Errorf("failed to create recording directory: %w", err)
	}

	fmt.Printf("Recording to directory: %s\n", recordingDir)
	var wg sync.WaitGroup
	errChan := make(chan error, len(cfg.Endpoints))

	// Start a proxy for each endpoint
	for _, endpoint := range cfg.Endpoints {
		wg.Add(1)
		go func(ep config.EndpointConfig) {
			defer wg.Done()

			// Create endpoint-specific directory
			endpointDir := filepath.Join(recordingDir, fmt.Sprintf("%s:%d", ep.TargetHost, ep.TargetPort))
			if err := os.MkdirAll(endpointDir, 0755); err != nil {
				errChan <- fmt.Errorf("failed to create endpoint directory: %w", err)
				return
			}

			var err error
			// Choose proxy type based on source and target types
			if strings.ToLower(ep.SourceType) == "http" && strings.ToLower(ep.TargetType) == "https" {
				err = proxyHTTPToHTTPS(ep, endpointDir)
			} else {
				err = proxyTCPEndpoint(ep)
			}

			if err != nil {
				errChan <- fmt.Errorf("proxy error for %s:%d: %w",
					ep.TargetHost, ep.TargetPort, err)
			}
		}(endpoint)
	}

	// Wait for all proxies to complete (they shouldn't unless there's an error)
	go func() {
		wg.Wait()
		close(errChan)
	}()

	// Return the first error encountered, if any
	for err := range errChan {
		return err
	}

	// Block forever (or until interrupted)
	select {}
}

// proxyHTTPToHTTPS handles HTTP to HTTPS proxying
func proxyHTTPToHTTPS(endpoint config.EndpointConfig, recordingDir string) error {
	// Create an HTTP server
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", endpoint.SourcePort),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// First read the entire body
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, fmt.Sprintf("Error reading request body: %v", err), http.StatusInternalServerError)
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

			// Dump the request for hashing and logging
			reqDump, err := httputil.DumpRequest(reqCopy, true)
			if err == nil {
				fmt.Printf("[HTTP->HTTPS] Received request:\n%s\n", string(reqDump))
			}

			// Calculate SHA256 hash of the request (with redacted headers removed)
			hasher := sha256.New()
			hasher.Write(reqDump)
			requestHash := hex.EncodeToString(hasher.Sum(nil))

			// Log the path for easier identification
			fmt.Printf("[HTTP->HTTPS] Processing request: %s %s (hash: %s)\n",
				r.Method, r.URL.Path, requestHash)

			// Save request to file
			if err := os.WriteFile(filepath.Join(recordingDir, requestHash+".req"), reqDump, 0644); err != nil {
				fmt.Printf("Error saving request to file: %v\n", err)
			} else {
				fmt.Printf("Saved request to %s.req\n", requestHash)
			}

			// Create a new request with the same method, URL, and body
			url := fmt.Sprintf("https://%s:%d%s", endpoint.TargetHost, endpoint.TargetPort, r.URL.Path)
			if r.URL.RawQuery != "" {
				url += "?" + r.URL.RawQuery
			}

			proxyReq, err := http.NewRequest(r.Method, url, bytes.NewReader(bodyBytes))
			if err != nil {
				http.Error(w, fmt.Sprintf("Error creating proxy request: %v", err), http.StatusInternalServerError)
				return
			}

			// Copy all headers from original request
			for name, values := range r.Header {
				for _, value := range values {
					proxyReq.Header.Add(name, value)
				}
			}
			// Create a custom transport with TLS configuration
			transport := &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // Skip certificate verification
				},
			}

			// Send the request
			client := &http.Client{Transport: transport}
			resp, err := client.Do(proxyReq)
			if err != nil {
				http.Error(w, fmt.Sprintf("Error sending proxy request: %v", err), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()

			// Read the response body
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				http.Error(w, fmt.Sprintf("Error reading response body: %v", err), http.StatusInternalServerError)
				return
			}

			// Check if response is gzipped
			var processedBody []byte
			isGzipped := false
			for _, value := range resp.Header["Content-Encoding"] {
				if strings.Contains(strings.ToLower(value), "gzip") {
					isGzipped = true
					break
				}
			}

			// Decompress if gzipped
			if isGzipped {
				gzipReader, err := gzip.NewReader(bytes.NewReader(respBody))
				if err != nil {
					fmt.Printf("Error creating gzip reader: %v\n", err)
					processedBody = respBody // Use original body if decompression fails
				} else {
					defer gzipReader.Close()
					processedBody, err = io.ReadAll(gzipReader)
					if err != nil {
						fmt.Printf("Error decompressing gzipped response: %v\n", err)
						processedBody = respBody // Use original body if decompression fails
					} else {
						fmt.Printf("[HTTP->HTTPS] Decompressed gzipped response (%d -> %d bytes)\n",
							len(respBody), len(processedBody))
					}
				}
			} else {
				processedBody = respBody
			}

			// Log the response headers
			fmt.Printf("[HTTP->HTTPS] Received response headers:\n")
			for name, values := range resp.Header {
				for _, value := range values {
					fmt.Printf("%s: %s\n", name, value)
				}
			}

			// Log the response body in a readable format
			fmt.Printf("[HTTP->HTTPS] Received response body (%d bytes):\n%s\n",
				len(processedBody), formatData(processedBody))

			// Save the response to a file
			respDump, err := httputil.DumpResponse(resp, false) // Headers only
			if err != nil {
				fmt.Printf("Error dumping response: %v\n", err)
			} else {
				// Create response with headers and decompressed body
				fullResponse := append(respDump, processedBody...)

				// Save to file
				filename := filepath.Join(recordingDir, requestHash)
				if err := os.WriteFile(filename, fullResponse, 0644); err != nil {
					fmt.Printf("Error saving response to file: %v\n", err)
				} else {
					fmt.Printf("Saved response to %s\n", filename)
				}

				// If the response was gzipped, also save the original compressed body
				if isGzipped {
					// Save the original compressed body to a separate file
					compressedFilename := filename + ".gz"
					if err := os.WriteFile(compressedFilename, respBody, 0644); err != nil {
						fmt.Printf("Error saving compressed response to file: %v\n", err)
					} else {
						fmt.Printf("Saved compressed response to %s\n", compressedFilename)
					}
				}
			}
			// Copy response headers
			for name, values := range resp.Header {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}

			// Set status code
			w.WriteHeader(resp.StatusCode)

			// Write the response body
			w.Write(respBody) // Send original (compressed) body to client
		}),
	}

	fmt.Printf("HTTP->HTTPS proxy listening on :%d, forwarding to https://%s:%d\n",
		endpoint.SourcePort, endpoint.TargetHost, endpoint.TargetPort)

	return server.ListenAndServe()
}

func handleHTTPRequest(w http.ResponseWriter, r *http.Request, endpoint config.EndpointConfig, recordingDir string) error {
	// Read and process the request
	requestHash, bodyBytes, err := processRequest(r, endpoint)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error processing request: %v", err), http.StatusInternalServerError)
		return err
	}

	// Create and send the proxy request
	resp, err := createAndSendProxyRequest(r, endpoint, bodyBytes)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error sending proxy request: %v", err), http.StatusBadGateway)
		return err
	}
	defer resp.Body.Close()

	// Process the response
	processedBody, isGzipped, err := processResponse(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error processing response: %v", err), http.StatusInternalServerError)
		return err
	}

	// Save the response
	err = saveResponse(resp, processedBody, isGzipped, recordingDir, requestHash, bodyBytes)
	if err != nil {
		fmt.Printf("Error saving response: %v\n", err)
	}

	// Copy the response to the client
	copyResponseToClient(w, resp, processedBody)
	return nil
}

func processRequest(r *http.Request, endpoint config.EndpointConfig) (string, []byte, error) {
	// First read the entire body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, fmt.Errorf("error reading request body: %w", err)
	}
	r.Body.Close()

	// Create a copy of the request for hashing with the body restored
	reqCopy := r.Clone(r.Context())
	reqCopy.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Remove any redacted headers before hashing
	for _, header := range endpoint.RedactRequestHeaders {
		reqCopy.Header.Del(header)
	}

	// Dump the request for hashing and logging
	reqDump, err := httputil.DumpRequest(reqCopy, true)
	if err != nil {
		fmt.Printf("Error dumping request: %v\n", err)
	} else {
		fmt.Printf("[HTTP->HTTPS] Received request:\n%s\n", string(reqDump))
	}

	// Calculate SHA256 hash of the request (with redacted headers removed)
	hasher := sha256.New()
	hasher.Write(reqDump)
	requestHash := hex.EncodeToString(hasher.Sum(nil))

	// Log the path for easier identification
	fmt.Printf("[HTTP->HTTPS] Processing request: %s %s (hash: %s)\n",
		r.Method, r.URL.Path, requestHash)

	return requestHash, bodyBytes, nil
}

func createAndSendProxyRequest(r *http.Request, endpoint config.EndpointConfig, bodyBytes []byte) (*http.Response, error) {
	// Create a new request with the same method, URL, and body
	url := fmt.Sprintf("https://%s:%d%s", endpoint.TargetHost, endpoint.TargetPort, r.URL.Path)
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequest(r.Method, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error creating proxy request: %w", err)
	}

	// Copy all headers from original request
	for name, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(name, value)
		}
	}

	// Create a custom transport with TLS configuration
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // Skip certificate verification
		},
	}

	// Send the request
	client := &http.Client{Transport: transport}
	resp, err := client.Do(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("error sending proxy request: %w", err)
	}

	return resp, nil
}

func processResponse(resp *http.Response) ([]byte, bool, error) {
	// Read the response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("error reading response body: %w", err)
	}

	// Check if response is gzipped
	var processedBody []byte
	isGzipped := false
	for _, value := range resp.Header["Content-Encoding"] {
		if strings.Contains(strings.ToLower(value), "gzip") {
			isGzipped = true
			break
		}
	}

	// Decompress if gzipped
	if isGzipped {
		gzipReader, err := gzip.NewReader(bytes.NewReader(respBody))
		if err != nil {
			fmt.Printf("Error creating gzip reader: %v\n", err)
			processedBody = respBody // Use original body if decompression fails
		} else {
			defer gzipReader.Close()
			processedBody, err = io.ReadAll(gzipReader)
			if err != nil {
				fmt.Printf("Error decompressing gzipped response: %v\n", err)
				processedBody = respBody // Use original body if decompression fails
			} else {
				fmt.Printf("[HTTP->HTTPS] Decompressed gzipped response (%d -> %d bytes)\n",
					len(respBody), len(processedBody))
			}
		}
	} else {
		processedBody = respBody
	}

	// Log the response headers
	fmt.Printf("[HTTP->HTTPS] Received response headers:\n")
	for name, values := range resp.Header {
		for _, value := range values {
			fmt.Printf("%s: %s\n", name, value)
		}
	}

	// Log the response body in a readable format
	fmt.Printf("[HTTP->HTTPS] Received response body (%d bytes):\n%s\n",
		len(processedBody), formatData(processedBody))

	return processedBody, isGzipped, nil
}

func saveResponse(resp *http.Response, processedBody []byte, isGzipped bool, recordingDir string, requestHash string, respBody []byte) error {
	respDump, err := httputil.DumpResponse(resp, false) // Headers only
	if err != nil {
		return fmt.Errorf("error dumping response: %w", err)
	}

	// Create response with headers and decompressed body
	fullResponse := append(respDump, processedBody...)

	// Save to file
	filename := filepath.Join(recordingDir, requestHash)
	if err := os.WriteFile(filename, fullResponse, 0644); err != nil {
		return fmt.Errorf("error saving response to file: %w", err)
	} else {
		fmt.Printf("Saved response to %s\n", filename)
	}

	// If the response was gzipped, also save the original compressed body
	if isGzipped {
		// Save the original compressed body to a separate file
		compressedFilename := filename + ".gz"
		if err := os.WriteFile(compressedFilename, respBody, 0644); err != nil {
			return fmt.Errorf("error saving compressed response to file: %w", err)
		} else {
			fmt.Printf("Saved compressed response to %s\n", compressedFilename)
		}
	}
	return nil
}

func copyResponseToClient(w http.ResponseWriter, resp *http.Response, processedBody []byte) {
	// Copy response headers
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	// Set status code
	w.WriteHeader(resp.StatusCode)

	// Write the response body
	w.Write(processedBody) // Send original (compressed) body to client
}

// Rename the original function to indicate it's for TCP proxying
func proxyTCPEndpoint(endpoint config.EndpointConfig) error {
	// Create listener on source port
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", endpoint.SourcePort))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", endpoint.SourcePort, err)
	}
	defer listener.Close()

	fmt.Printf("TCP proxy listening on :%d, forwarding to %s:%d\n",
		endpoint.SourcePort, endpoint.TargetHost, endpoint.TargetPort)

	// Accept connections
	for {
		clientConn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("failed to accept connection: %w", err)
		}

		// Handle each connection in a goroutine
		go handleTCPConnection(clientConn, endpoint)
	}
}

// Rename the original function to indicate it's for TCP connections
func handleTCPConnection(clientConn net.Conn, endpoint config.EndpointConfig) {
	defer clientConn.Close()

	// Connect to target
	targetConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", endpoint.TargetHost, endpoint.TargetPort))
	if err != nil {
		fmt.Printf("Failed to connect to target %s:%d: %v\n",
			endpoint.TargetHost, endpoint.TargetPort, err)
		return
	}
	defer targetConn.Close()

	connID := fmt.Sprintf("%s-%d", clientConn.RemoteAddr(), time.Now().UnixNano())
	fmt.Printf("[%s] New TCP connection: localhost:%d -> %s:%d\n",
		connID, endpoint.SourcePort, endpoint.TargetHost, endpoint.TargetPort)

	// Proxy data in both directions
	var wg sync.WaitGroup
	wg.Add(2)

	// Client -> Target
	go func() {
		defer wg.Done()
		copyAndLog(targetConn, clientConn, connID, "REQUEST")
	}()

	// Target -> Client
	go func() {
		defer wg.Done()
		copyAndLog(clientConn, targetConn, connID, "RESPONSE")
	}()

	// Wait for both directions to complete
	wg.Wait()
	fmt.Printf("[%s] TCP connection closed: localhost:%d -> %s:%d\n",
		connID, endpoint.SourcePort, endpoint.TargetHost, endpoint.TargetPort)
}

// copyAndLog copies data from src to dst and logs it
func copyAndLog(dst io.Writer, src io.Reader, connID, direction string) {
	// Create a buffer to store the data
	buf := new(bytes.Buffer)

	// Copy data from source to both destination and buffer
	n, err := io.Copy(io.MultiWriter(dst, buf), src)
	if err != nil {
		fmt.Printf("[%s] Error copying %s data: %v\n", connID, direction, err)
		return
	}

	// Log the data
	if n > 0 {
		fmt.Printf("[%s] %s (%d bytes):\n%s\n",
			connID, direction, n, formatData(buf.Bytes()))
	}
}

// formatData formats data for logging, handling both text and binary data
func formatData(data []byte) string {
	// If it looks like text, return it as-is (up to a reasonable length)
	if isTextData(data) {
		if len(data) > 1024 {
			return string(data[:1024]) + "... [truncated]"
		}
		return string(data)
	}

	// Otherwise format as hex (truncated for large payloads)
	maxBytes := 256
	if len(data) > maxBytes {
		return fmt.Sprintf("%X... [truncated %d bytes]", data[:maxBytes], len(data)-maxBytes)
	}
	return fmt.Sprintf("%X", data)
}

// isTextData makes a best guess if data is text
func isTextData(data []byte) bool {
	// Simple heuristic: if the data contains mostly printable ASCII characters
	// and no NUL bytes, it's probably text
	if len(data) == 0 {
		return true
	}

	textCount := 0
	for _, b := range data {
		if b == 0 {
			return false // NUL byte indicates binary
		}
		if (b >= 32 && b <= 126) || b == '\n' || b == '\r' || b == '\t' {
			textCount++
		}
	}

	// If more than 85% is printable text, consider it text
	return textCount > len(data)*85/100
}
