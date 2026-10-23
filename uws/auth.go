/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package uws

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/urpc/uio/uws/internal/handshake"
)

// RejectError is what Server.Authorize returns to refuse an upgrade with a
// response of its choosing. Status is the HTTP status, 403 when zero or not a
// 4xx or 5xx code; Header is added to the response; Reason is its plain-text
// body, cut to 4 KiB, which a uws client reports as HandshakeError.Reason.
type RejectError struct {
	Status int
	Header http.Header
	Reason string
}

func (e *RejectError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("uws: upgrade rejected with %d: %s", e.status(), e.Reason)
	}
	return fmt.Sprintf("uws: upgrade rejected with %d", e.status())
}

func (e *RejectError) status() int {
	if e.Status < 400 || e.Status > 599 {
		return http.StatusForbidden
	}
	return e.Status
}

// HandshakeError reports a server that refused the upgrade: it answered with
// a status other than 101. A Dialer's OnClose gets it as CloseEvent.Err, and
// DialClient returns it, wrapped in a CloseError, so errors.As finds it.
type HandshakeError struct {
	StatusCode int
	Header     http.Header
	// Reason is the response body, when the server sent a short one; a uws
	// server sends RejectError.Reason there.
	Reason string
}

func (e *HandshakeError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("uws: handshake refused with status %d: %s", e.StatusCode, e.Reason)
	}
	return fmt.Sprintf("uws: handshake refused with status %d", e.StatusCode)
}

// rejectResponse is the server's answer to a handshake that failed with err:
// what an Authorize RejectError asks for, 403 for any other Authorize error,
// and 400 for a request that is not a valid upgrade.
func rejectResponse(err error) []byte {
	var reject *RejectError
	if errors.As(err, &reject) {
		header := reject.Header
		if handshake.CheckRejectHeader(header) != nil {
			header = nil
		}
		return handshake.BuildRejectResponse(reject.status(), header, reject.Reason)
	}
	var denied *authorizeError
	if errors.As(err, &denied) {
		return handshake.BuildRejectResponse(http.StatusForbidden, nil, "")
	}
	return handshake.BuildRejectResponse(http.StatusBadRequest, nil, "")
}

// authorizeError marks an error Server.Authorize returned.
type authorizeError struct{ err error }

func (e *authorizeError) Error() string { return "uws: upgrade not authorized: " + e.err.Error() }
func (e *authorizeError) Unwrap() error { return e.err }

// clientHandshakeError turns a server's refusal into a HandshakeError.
func clientHandshakeError(err error) error {
	var refused *handshake.ResponseError
	if !errors.As(err, &refused) {
		return err
	}
	return &HandshakeError{StatusCode: refused.StatusCode, Header: refused.Header, Reason: string(refused.Body)}
}
