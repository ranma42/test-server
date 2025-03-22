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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type SHA256Sum [32]byte

func HeadSha() [32]byte {
	return [32]byte{
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
}

// NewRecordedRequest creates a RecordedRequest from an http.Request.
func NewRecordedRequest(req *http.Request, previousRequest SHA256Sum) (*RecordedRequest, error) {
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
