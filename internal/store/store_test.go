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
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecordedRequest_Serialize(t *testing.T) {
	testCases := []struct {
		name     string
		request  RecordedRequest
		expected string
	}{
		{
			name: "Empty request",
			request: RecordedRequest{
				Request:         "",
				Header:          http.Header{},
				Body:            []byte{},
				PreviousRequest: [32]byte{},
			},
			expected: "0000000000000000000000000000000000000000000000000000000000000000\n\n\n\n",
		},
		{
			name: "Request with headers",
			request: RecordedRequest{
				Request: "GET / HTTP/1.1",
				Header: http.Header{
					"Accept":       []string{"application/xml"},
					"Content-Type": []string{"application/json"},
				},
				Body:            []byte{},
				PreviousRequest: [32]byte{},
			},
			expected: "0000000000000000000000000000000000000000000000000000000000000000\nGET / HTTP/1.1\nAccept: application/xml\nContent-Type: application/json\n\n\n",
		},
		{
			name: "Request with body",
			request: RecordedRequest{
				Request:         "POST /data HTTP/1.1",
				Header:          http.Header{},
				Body:            []byte("{\"key\": \"value\"}"),
				PreviousRequest: [32]byte{},
			},
			expected: "0000000000000000000000000000000000000000000000000000000000000000\nPOST /data HTTP/1.1\n\n\n{\"key\": \"value\"}",
		},
		{
			name: "Request with previous request SHA256 sum",
			request: RecordedRequest{
				Request:         "GET / HTTP/1.1",
				Header:          http.Header{},
				Body:            []byte{},
				PreviousRequest: [32]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20},
			},
			expected: "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20\nGET / HTTP/1.1\n\n\n",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := tc.request.Serialize()
			require.Equal(t, tc.expected, actual, "Serialize() result mismatch")
		})
	}
}

func TestNewRecordedRequest(t *testing.T) {
	headSha := [32]byte{
		0xf7, 0x63, 0x4a, 0xb5, 0x2b, 0xfb, 0x7b, 0xfb,
		0xa7, 0xca, 0xc0, 0x1c, 0xe2, 0xae, 0xb9, 0x6a,
		0xde, 0x85, 0x4e, 0x8d, 0xfe, 0x9c, 0x5e, 0x9b,
		0xfb, 0x1a, 0x72, 0x62, 0xcf, 0xa5, 0x0e, 0x49,
	}

	tests := []struct {
		name        string
		request     *http.Request
		expected    *RecordedRequest
		expectedErr bool
	}{
		{
			name: "Test with body",
			request: func() *http.Request {
				req, _ := http.NewRequest("POST", "http://example.com/test", bytes.NewBuffer([]byte("test body")))
				req.Header.Set("Content-Type", "application/json")
				return req
			}(),
			expected: &RecordedRequest{
				Request:         "POST http://example.com/test HTTP/1.1",
				Header:          http.Header{"Content-Type": []string{"application/json"}},
				Body:            []byte("test body"),
				PreviousRequest: headSha,
			},
			expectedErr: false,
		},
		{
			name: "Test without body",
			request: func() *http.Request {
				req, _ := http.NewRequest("GET", "http://example.com/test", nil)
				return req
			}(),
			expected: &RecordedRequest{
				Request:         "GET http://example.com/test HTTP/1.1",
				Header:          http.Header{},
				Body:            []byte{},
				PreviousRequest: headSha,
			},
			expectedErr: false,
		},
		{
			name: "Test with error reading body",
			request: func() *http.Request {
				req, _ := http.NewRequest("POST", "http://example.com/test", &errorReader{})
				return req
			}(),
			expected:    nil,
			expectedErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recordedRequest, err := NewRecordedRequest(tc.request, headSha)

			if tc.expectedErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expected.Request, recordedRequest.Request)
			require.Equal(t, tc.expected.Header, recordedRequest.Header)
			require.Equal(t, tc.expected.Body, recordedRequest.Body)
			require.Equal(t, tc.expected.PreviousRequest, recordedRequest.PreviousRequest)
		})
	}
}

type errorReader struct{}

func (e *errorReader) Read(p []byte) (n int, err error) {
	return 0, fmt.Errorf("simulated error")
}
