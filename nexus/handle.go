package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const getResultContextPadding = time.Second * 5

// An OperationHandle is used to cancel operations and get their result and status.
type OperationHandle[T any] struct {
	// Name of the Operation this handle represents.
	Operation string
	// Handler generated ID for this handle's operation.
	ID     string
	client *Client
}

// GetInfo gets operation information, issuing a network request to the service handler.
func (h *OperationHandle[T]) GetInfo(ctx context.Context, options GetOperationInfoOptions) (*OperationInfo, error) {
	url := h.client.serviceBaseURL.JoinPath(url.PathEscape(h.client.options.Service), url.PathEscape(h.Operation), url.PathEscape(h.ID))
	request, err := http.NewRequestWithContext(ctx, "GET", url.String(), nil)
	if err != nil {
		return nil, err
	}
	addContextTimeoutToHTTPHeader(ctx, request.Header)
	addNexusHeaderToHTTPHeader(options.Header, request.Header)

	request.Header.Set(headerUserAgent, userAgent)
	response, err := h.client.options.HTTPCaller(request)
	if err != nil {
		return nil, err
	}

	// Do this once here and make sure it doesn't leak.
	body, err := readAndReplaceBody(response)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		return nil, bestEffortHandlerErrorFromResponse(response, body)
	}

	return operationInfoFromResponse(response, body)
}

// GetResult gets the result of an operation, issuing a network request to the service handler.
//
// By default, GetResult returns (nil, [ErrOperationStillRunning]) immediately after issuing a call if the operation has
// not yet completed.
//
// Callers may set GetOperationResultOptions.Wait to a value greater than 0 to alter this behavior, causing the client
// to long poll for the result issuing one or more requests until the provided wait period exceeds, in which case (nil,
// [ErrOperationStillRunning]) is returned.
//
// The wait time is capped to the deadline of the provided context. Make sure to handle both context deadline errors and
// [ErrOperationStillRunning].
//
// Note that the wait period is enforced by the server and may not be respected if the server is misbehaving. Set the
// context deadline to the max allowed wait period to ensure this call returns in a timely fashion.
//
// ⚠️ If a [LazyValue] is returned (as indicated by T), it must be consumed to free up the underlying connection.
func (h *OperationHandle[T]) GetResult(ctx context.Context, options GetOperationResultOptions) (T, error) {
	var result T
	url := h.client.serviceBaseURL.JoinPath(url.PathEscape(h.client.options.Service), url.PathEscape(h.Operation), url.PathEscape(h.ID), "result")
	request, err := http.NewRequestWithContext(ctx, "GET", url.String(), nil)
	if err != nil {
		return result, err
	}
	addContextTimeoutToHTTPHeader(ctx, request.Header)
	request.Header.Set(headerUserAgent, userAgent)
	addNexusHeaderToHTTPHeader(options.Header, request.Header)

	startTime := time.Now()
	wait := options.Wait
	for {
		if wait > 0 {
			if deadline, set := ctx.Deadline(); set {
				// Ensure we don't wait longer than the deadline but give some buffer prevent racing between wait and
				// context deadline.
				wait = min(wait, time.Until(deadline)+getResultContextPadding)
			}

			q := request.URL.Query()
			q.Set(queryWait, fmt.Sprintf("%dms", wait.Milliseconds()))
			request.URL.RawQuery = q.Encode()
		} else {
			// We may reuse the request object multiple times and will need to reset the query when wait becomes 0 or
			// negative.
			request.URL.RawQuery = ""
		}

		response, err := h.sendGetOperationRequest(ctx, request)
		if err != nil {
			if wait > 0 && errors.Is(err, errOperationWaitTimeout) {
				// TODO: Backoff a bit in case the server is continually returning timeouts due to some LB configuration
				// issue to avoid blowing it up with repeated calls.
				wait = options.Wait - time.Since(startTime)
				continue
			}
			return result, err
		}
		s := &LazyValue{
			serializer: h.client.options.Serializer,
			Reader: &Reader{
				response.Body,
				prefixStrippedHTTPHeaderToNexusHeader(response.Header, "content-"),
			},
		}
		if _, ok := any(result).(*LazyValue); ok {
			return any(s).(T), nil
		} else {
			return result, s.Consume(&result)
		}
	}
}

func (h *OperationHandle[T]) sendGetOperationRequest(ctx context.Context, request *http.Request) (*http.Response, error) {
	response, err := h.client.options.HTTPCaller(request)
	if err != nil {
		return nil, err
	}

	if response.StatusCode == http.StatusOK {
		return response, nil
	}

	// Do this once here and make sure it doesn't leak.
	body, err := readAndReplaceBody(response)
	if err != nil {
		return nil, err
	}

	switch response.StatusCode {
	case http.StatusRequestTimeout:
		return nil, errOperationWaitTimeout
	case statusOperationRunning:
		return nil, ErrOperationStillRunning
	case statusOperationFailed:
		state, err := getUnsuccessfulStateFromHeader(response, body)
		if err != nil {
			return nil, err
		}
		failure, err := failureFromResponse(response, body)
		if err != nil {
			return nil, err
		}
		return nil, &UnsuccessfulOperationError{
			State:   state,
			Failure: failure,
		}
	default:
		return nil, bestEffortHandlerErrorFromResponse(response, body)
	}
}

// Cancel requests to cancel an asynchronous operation.
//
// Cancelation is asynchronous and may be not be respected by the operation's implementation.
func (h *OperationHandle[T]) Cancel(ctx context.Context, options CancelOperationOptions) error {
	url := h.client.serviceBaseURL.JoinPath(url.PathEscape(h.client.options.Service), url.PathEscape(h.Operation), url.PathEscape(h.ID), "cancel")
	request, err := http.NewRequestWithContext(ctx, "POST", url.String(), nil)
	if err != nil {
		return err
	}
	addContextTimeoutToHTTPHeader(ctx, request.Header)
	request.Header.Set(headerUserAgent, userAgent)
	addNexusHeaderToHTTPHeader(options.Header, request.Header)
	response, err := h.client.options.HTTPCaller(request)
	if err != nil {
		return err
	}

	// Do this once here and make sure it doesn't leak.
	body, err := readAndReplaceBody(response)
	if err != nil {
		return err
	}

	if response.StatusCode != http.StatusAccepted {
		return bestEffortHandlerErrorFromResponse(response, body)
	}
	return nil
}
