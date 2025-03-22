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

package store

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/test-server/internal/config"
)

type SHA256Sum [32]byte

func HeadSHA() SHA256Sum {
	return SHA256Sum{
		0xf7, 0x63, 0x4a, 0xb5, 0x2b, 0xfb, 0x7b, 0xfb,
		0xa7, 0xca, 0xc0, 0x1c, 0xe2, 0xae, 0xb9, 0x6a,
		0xde, 0x85, 0x4e, 0x8d, 0xfe, 0x9c, 0x5e, 0x9b,
		0xfb, 0x1a, 0x72, 0x62, 0xcf, 0xa5, 0x0e, 0x49,
	}
}

type RecordedRequest struct {
	Request         string
	Header          http.Header
	Body            []byte
	PreviousRequest SHA256Sum
	ServerAddress   string
	Port            int64
	Protocol        string
}

// NewRecordedRequest creates a RecordedRequest from an http.Request.
func NewRecordedRequest(req *http.Request, previousRequest SHA256Sum, cfg config.EndpointConfig) (*RecordedRequest, error) {
	// Read the body.
	body, err := readBody(req)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	// Create the request string.
	request := fmt.Sprintf("%s %s %s", req.Method, req.URL.String(), req.Proto)

	// Create a copy of the headers.
	header := req.Header.Clone()

	// Create the RecordedRequest.
	recordedRequest := &RecordedRequest{
		Request:         request,
		Header:          header,
		Body:            body,
		PreviousRequest: previousRequest,
		ServerAddress:   cfg.TargetHost,
		Port:            cfg.TargetPort,
		Protocol:        cfg.TargetType,
	}

	return recordedRequest, nil
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return []byte{}, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	// Restore the request body for further use.
	req.Body = io.NopCloser(bytes.NewBuffer(body))
	return body, nil
}

// ComputeSum computes the SHA256 sum of a RecordedRequest.
func (r *RecordedRequest) ComputeSum() (SHA256Sum, error) {
	// Serialize the header and body into a byte stream.
	headerBytes, err := json.Marshal(r.Header)
	if err != nil {
		return SHA256Sum{}, fmt.Errorf("failed to marshal header: %w", err)
	}

	data := bytes.Join([][]byte{headerBytes, r.Body}, []byte{})

	// Compute the SHA256 hash.
	hash := sha256.Sum256(data)
	return hash, nil
}

// Serialize the request.
//
// The serialization format is as follows
//   - The first line is the sha256 of the previous request as a hex string.
//   - Next is the HTTP request.
//   - Next there's a single line for each header formatted as "{key}: {value}".
//   - Next there are 2 empty lines.
//   - Rest of the file is the body content.
func (r *RecordedRequest) Serialize() string {
	var builder strings.Builder

	// Format the SHA256 sum of the previous request.
	previousRequestSum := hex.EncodeToString(r.PreviousRequest[:])
	builder.WriteString(previousRequestSum)
	builder.WriteString("\n")

	builder.WriteString(fmt.Sprintf("Server Address: %s\n", r.ServerAddress))

	builder.WriteString(fmt.Sprintf("Port: %d\n", r.Port))

	builder.WriteString(fmt.Sprintf("Protocol: %s\n", r.Protocol))

	builder.WriteString(strings.Repeat("*", 80) + "\n")

	// Format the HTTP request line.
	builder.WriteString(r.Request)
	builder.WriteString("\n")

	// Format the headers.

	for key, values := range r.Header {
		for _, value := range values {
			builder.WriteString(fmt.Sprintf("%s: %s\n", key, value))
		}
	}

	builder.WriteString("\n\n")
	builder.WriteString(string(r.Body))

	return builder.String()
}

// Deserialize the request.
func Deserialize(data string) (*RecordedRequest, error) {
	lines := strings.Split(data, "\n")
	if len(lines) < 6 {
		return nil, fmt.Errorf("invalid serialized data: not enough lines")
	}

	previousRequestSum, err := hex.DecodeString(lines[0])
	if err != nil {
		return nil, fmt.Errorf("invalid previous request sum: %w", err)
	}

	var previousRequest SHA256Sum
	copy(previousRequest[:], previousRequestSum)

	serverAddress := strings.TrimPrefix(lines[1], "Server Address: ")
	portString := strings.TrimPrefix(lines[2], "Port: ")
	protocol := strings.TrimPrefix(lines[3], "Protocol: ")

	port := 0
	if portString != "" {
		_, err = fmt.Sscan(portString, &port)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %w", err)
		}
	}

	request := lines[5]

	headerStart := 6
	bodyStart := -1
	headers := make(http.Header)

	for i := headerStart; i < len(lines); i++ {
		if lines[i] == "" {
			bodyStart = i + 1
			break
		}
		parts := strings.SplitN(lines[i], ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		value := parts[1]
		headers.Add(key, value)
	}

	var body []byte
	if bodyStart != -1 && bodyStart < len(lines) {
		body = []byte(strings.Join(lines[bodyStart:], "\n"))
	}

	recordedRequest := &RecordedRequest{
		Request:         request,
		Header:          headers,
		Body:            body,
		PreviousRequest: previousRequest,
		ServerAddress:   serverAddress,
		Port:            int64(port),
		Protocol:        protocol,
	}

	return recordedRequest, nil
}

// RedactHeaders removes the specified headers from the RecordedRequest.
func (r *RecordedRequest) RedactHeaders(headers []string) {
	for _, header := range headers {
		r.Header.Del(header)
	}
}

type RecordedResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func NewRecordedResponse(resp *http.Response, body []byte) (*RecordedResponse, error) {
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()

		// Read the uncompressed body.
		uncompressedBody := new(bytes.Buffer)
		_, err = uncompressedBody.ReadFrom(gzipReader)
		if err != nil {
			return nil, err
		}
		body = uncompressedBody.Bytes()

	}

	recordedResponse := &RecordedResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       body,
	}
	return recordedResponse, nil
}

func (r *RecordedResponse) Serialize() string {
	var buffer bytes.Buffer

	buffer.WriteString(fmt.Sprintf("Status code: %d \n", r.StatusCode))
	for name, values := range r.Header {
		for _, value := range values {
			buffer.WriteString(fmt.Sprintf("%s: %s\n", name, value))
		}
	}
	buffer.WriteString("\n")
	buffer.Write(r.Body)

	return buffer.String()
}
