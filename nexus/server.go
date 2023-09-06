package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/gorilla/mux"
)

// StartOperationRequest is input for Handler.StartOperation.
type StartOperationRequest struct {
	// Operation name.
	Operation string
	// Request ID, should be used to dedupe start requests.
	RequestID string
	// Callback URL to call upon completion if the started operation is async.
	CallbackURL string
	// The original HTTP request.
	// Read the URL, Header, and Body of the request to process the operation input.
	HTTPRequest *http.Request
}

// GetOperationResultRequest is input for Handler.GetOperationResult.
type GetOperationResultRequest struct {
	// Operation name.
	Operation string
	// Operation ID as originally generated by a Handler.
	// It is the handler's responsibility to validate this ID and authorize access to the underlying resource.
	OperationID string
	// If non-zero, reflects the duration the caller has indicated that it wants to wait for operation completion,
	// turning the request into a long poll.
	Wait time.Duration
	// The original HTTP request.
	HTTPRequest *http.Request
}

// GetOperationInfoRequest is input for Handler.GetOperationInfo.
type GetOperationInfoRequest struct {
	// Operation name.
	Operation string
	// Operation ID as originally generated by a Handler.
	// It is the handler's responsibility to validate this ID and authorize access to the underlying resource.
	OperationID string
	// The original HTTP request.
	HTTPRequest *http.Request
}

// CancelOperationRequest is input for Handler.CancelOperation.
type CancelOperationRequest struct {
	// Operation name.
	Operation string
	// Operation ID as originally generated by a Handler.
	// It is the handler's responsibility to validate this ID and authorize access to the underlying resource.
	OperationID string
	// The original HTTP request.
	HTTPRequest *http.Request
}

// An OperationResponse is the return type from the handler StartOperation and GetResult methods. It has two
// implementations: [OperationResponseSync] and [OperationResponseAsync].
type OperationResponse interface {
	applyToHTTPResponse(http.ResponseWriter, *httpHandler)
}

// Indicates that an operation completed successfully.
type OperationResponseSync struct {
	// Header to deliver in the HTTP response.
	Header http.Header
	// Body conveying the operation result.
	// If it is an [io.Closer] it will be automatically closed by the framework.
	Body io.Reader
}

// NewOperationResponseSync constructs an [OperationResponseSync], setting the proper Content-Type header.
// Marhsals the provided value to JSON using [json.Marshal].
func NewOperationResponseSync(v any) (*OperationResponseSync, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	header := make(http.Header)
	header.Set(headerContentType, contentTypeJSON)
	return &OperationResponseSync{
		Header: header,
		Body:   bytes.NewReader(b),
	}, nil
}

func (r *OperationResponseSync) applyToHTTPResponse(writer http.ResponseWriter, handler *httpHandler) {
	header := writer.Header()
	for k, v := range r.Header {
		header[k] = v
	}
	if closer, ok := r.Body.(io.Closer); ok {
		defer closer.Close()
	}
	if _, err := io.Copy(writer, r.Body); err != nil {
		handler.logger.Error("failed to write response body", "error", err)
	}
}

// Indicates that an operation has been accepted and will complete asynchronously.
type OperationResponseAsync struct {
	OperationID string
}

func (r *OperationResponseAsync) applyToHTTPResponse(writer http.ResponseWriter, handler *httpHandler) {
	info := OperationInfo{
		ID:    r.OperationID,
		State: OperationStateRunning,
	}
	bytes, err := json.Marshal(info)
	if err != nil {
		handler.logger.Error("failed to serialize operation info", "error", err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}

	writer.Header().Set(headerContentType, contentTypeJSON)
	writer.WriteHeader(http.StatusCreated)

	if _, err := writer.Write(bytes); err != nil {
		handler.logger.Error("failed to write response body", "error", err)
	}
}

// A Handler must implement all of the Nexus service endpoints as defined in the [Nexus HTTP API].
//
// Handler implementations must embed the [UnimplementedHandler].
//
// All Handler methods can return a [HandlerError] to fail requests with a custom status code and structured [Failure].
//
// [Nexus HTTP API]: https://github.com/nexus-rpc/api
type Handler interface {
	// StartOperation handles requests for starting an operation. Return [OperationResponseSync] to respond successfully
	// - inline, or [OperationResponseAsync] to indicate that an asynchronous operation was started.
	// Return an [UnsuccessfulOperationError] to indicate that an operation completed as failed or canceled.
	StartOperation(context.Context, *StartOperationRequest) (OperationResponse, error)
	// GetOperationResult handles requests to get the result of an asynchronous operation. Return
	// [OperationResponseSync] to respond successfully - inline, or error with [ErrOperationStillRunning] to indicate
	// that an asynchronous operation is still running.
	// Return an [UnsuccessfulOperationError] to indicate that an operation completed as failed or canceled.
	//
	// When [GetOperationResultRequest.Wait] is greater than zero, this request should be treated as a long poll.
	// Long poll requests have a server side timeout, configurable via [HandlerOptions.GetResultTimeout], and exposed
	// via context deadline. The context deadline is decoupled from the application level Wait duration.
	//
	// It is the implementor's responsiblity to respect the client's wait duration and return in a timely fashion.
	// Consider using a derived context that enforces the wait timeout when implementing this method and return
	// [ErrOperationStillRunning] when that context expires as shown in the example.
	GetOperationResult(context.Context, *GetOperationResultRequest) (*OperationResponseSync, error)
	// GetOperationInfo handles requests to get information about an asynchronous operation.
	GetOperationInfo(context.Context, *GetOperationInfoRequest) (*OperationInfo, error)
	// CancelOperation handles requests to cancel an asynchronous operation.
	// Cancelation in Nexus is:
	//  1. asynchronous - returning from this method only ensures that cancelation is delivered, it may later be ignored
	//  by the underlying operation implemention.
	//  2. idempotent - implementors should ignore duplicate cancelations for the same operation.
	CancelOperation(context.Context, *CancelOperationRequest) error
	mustEmbedUnimplementedHandler()
}

// HandlerError is a special error that can be returned from [Handler] methods for failing an HTTP request with a custom
// status code and failure message.
type HandlerError struct {
	// Defaults to 500.
	StatusCode int
	// Failure to report back in the response. Optional.
	Failure *Failure
}

// Error implements the error interface.
func (e *HandlerError) Error() string {
	if e.Failure != nil {
		return fmt.Sprintf("handler error (%d): %s", e.StatusCode, e.Failure.Message)
	}
	return fmt.Sprintf("handler error (%d)", e.StatusCode)
}

func newBadRequestError(format string, args ...any) *HandlerError {
	return &HandlerError{
		StatusCode: http.StatusBadRequest,
		Failure: &Failure{
			Message: fmt.Sprintf(format, args...),
		},
	}
}

type baseHTTPHandler struct {
	logger *slog.Logger
}

type httpHandler struct {
	baseHTTPHandler
	options HandlerOptions
}

func (h *baseHTTPHandler) writeFailure(writer http.ResponseWriter, err error) {
	var failure *Failure
	var unsuccessfulError *UnsuccessfulOperationError
	var handlerError *HandlerError
	var operationState OperationState
	statusCode := http.StatusInternalServerError

	if errors.As(err, &unsuccessfulError) {
		operationState = unsuccessfulError.State
		failure = &unsuccessfulError.Failure
		statusCode = statusOperationFailed

		if operationState == OperationStateFailed || operationState == OperationStateCanceled {
			writer.Header().Set(headerOperationState, string(operationState))
		} else {
			h.logger.Error("unexpected operation state", "state", operationState)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if errors.As(err, &handlerError) {
		failure = handlerError.Failure
		statusCode = handlerError.StatusCode
	} else {
		failure = &Failure{
			Message: "internal server error",
		}
		h.logger.Error("handler failed", "error", err)
	}

	var bytes []byte
	if failure != nil {
		bytes, err = json.Marshal(failure)
		if err != nil {
			h.logger.Error("failed to marshal failure", "error", err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writer.Header().Set(headerContentType, contentTypeJSON)
	}

	writer.WriteHeader(statusCode)

	if _, err := writer.Write(bytes); err != nil {
		h.logger.Error("failed to write response body", "error", err)
	}
}

func (h *httpHandler) startOperation(writer http.ResponseWriter, request *http.Request) {
	operation, err := url.PathUnescape(path.Base(request.URL.RawPath))
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	handlerRequest := &StartOperationRequest{
		Operation:   operation,
		RequestID:   request.Header.Get(headerRequestID),
		CallbackURL: request.URL.Query().Get(queryCallbackURL),
		HTTPRequest: request,
	}
	response, err := h.options.Handler.StartOperation(request.Context(), handlerRequest)
	if err != nil {
		h.writeFailure(writer, err)
	} else {
		response.applyToHTTPResponse(writer, h)
	}
}

func (h *httpHandler) getOperationResult(writer http.ResponseWriter, request *http.Request) {
	// strip /result
	prefix, operationIDEscaped := path.Split(path.Dir(request.URL.RawPath))
	operationID, err := url.PathUnescape(operationIDEscaped)
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	operation, err := url.PathUnescape(path.Base(prefix))
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	handlerRequest := &GetOperationResultRequest{Operation: operation, OperationID: operationID, HTTPRequest: request}

	waitStr := request.URL.Query().Get(queryWait)
	ctx := request.Context()
	if waitStr != "" {
		waitDuration, err := time.ParseDuration(waitStr)
		if err != nil {
			h.logger.Warn("invalid wait duration query parameter", "wait", waitStr)
			h.writeFailure(writer, newBadRequestError("invalid wait query parameter"))
			return
		}
		handlerRequest.Wait = waitDuration
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(request.Context(), h.options.GetResultTimeout)
		defer cancel()
	}

	response, err := h.options.Handler.GetOperationResult(ctx, handlerRequest)
	if err != nil {
		if handlerRequest.Wait > 0 && ctx.Err() != nil {
			writer.WriteHeader(http.StatusRequestTimeout)
		} else if errors.Is(err, ErrOperationStillRunning) {
			writer.WriteHeader(statusOperationRunning)
		} else {
			h.writeFailure(writer, err)
		}
		return
	}
	response.applyToHTTPResponse(writer, h)
}

func (h *httpHandler) getOperationInfo(writer http.ResponseWriter, request *http.Request) {
	prefix, operationIDEscaped := path.Split(request.URL.RawPath)
	operationID, err := url.PathUnescape(operationIDEscaped)
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	operation, err := url.PathUnescape(path.Base(prefix))
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	handlerRequest := &GetOperationInfoRequest{Operation: operation, OperationID: operationID, HTTPRequest: request}

	info, err := h.options.Handler.GetOperationInfo(request.Context(), handlerRequest)
	if err != nil {
		h.writeFailure(writer, err)
		return
	}

	bytes, err := h.options.Marshaler(info)
	if err != nil {
		h.writeFailure(writer, fmt.Errorf("failed to marshal operation info: %w", err))
		return
	}
	writer.Header().Set(headerContentType, contentTypeJSON)
	if _, err := writer.Write(bytes); err != nil {
		h.logger.Error("failed to write response body", "error", err)
	}
}

func (h *httpHandler) cancelOperation(writer http.ResponseWriter, request *http.Request) {
	// strip /cancel
	prefix, operationIDEscaped := path.Split(path.Dir(request.URL.RawPath))
	operationID, err := url.PathUnescape(operationIDEscaped)
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	operation, err := url.PathUnescape(path.Base(prefix))
	if err != nil {
		h.writeFailure(writer, newBadRequestError("failed to parse URL path"))
		return
	}
	handlerRequest := &CancelOperationRequest{Operation: operation, OperationID: operationID, HTTPRequest: request}

	if err := h.options.Handler.CancelOperation(request.Context(), handlerRequest); err != nil {
		h.writeFailure(writer, err)
		return
	}

	writer.WriteHeader(http.StatusAccepted)
}

// HandlerOptions are options for [NewHTTPHandler].
type HandlerOptions struct {
	// Handler for handling service requests.
	Handler Handler
	// A stuctured logger.
	// Defaults to slog.Default().
	Logger *slog.Logger
	// Optional marshaler for marshaling objects to JSON.
	// Defaults to json.Marshal.
	Marshaler func(any) ([]byte, error)
	// Max duration to allow waiting for a single get result request.
	// Enforced if provided for requests with the wait query parameter set.
	//
	// Defaults to one minute.
	GetResultTimeout time.Duration
}

// NewHTTPHandler constructs an [http.Handler] from given options for handling Nexus service requests.
func NewHTTPHandler(options HandlerOptions) http.Handler {
	if options.Marshaler == nil {
		options.Marshaler = json.Marshal
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.GetResultTimeout == 0 {
		options.GetResultTimeout = time.Minute
	}
	handler := &httpHandler{
		baseHTTPHandler: baseHTTPHandler{
			logger: slog.Default(),
		},
		options: options,
	}

	router := mux.NewRouter().UseEncodedPath()
	router.HandleFunc("/{operation}", handler.startOperation).Methods("POST")
	router.HandleFunc("/{operation}/{operation_id}", handler.getOperationInfo).Methods("GET")
	router.HandleFunc("/{operation}/{operation_id}/result", handler.getOperationResult).Methods("GET")
	router.HandleFunc("/{operation}/{operation_id}/cancel", handler.cancelOperation).Methods("POST")
	return router
}
