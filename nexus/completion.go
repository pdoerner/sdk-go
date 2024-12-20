package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// NewCompletionHTTPRequest creates an HTTP request deliver an operation completion to a given URL.
func NewCompletionHTTPRequest(ctx context.Context, url string, completion OperationCompletion) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, err
	}
	if err := completion.applyToHTTPRequest(httpReq); err != nil {
		return nil, err
	}

	httpReq.Header.Set(headerUserAgent, userAgent)
	return httpReq, nil
}

// OperationCompletion is input for [NewCompletionHTTPRequest].
// It has two implementations: [OperationCompletionSuccessful] and [OperationCompletionUnsuccessful].
type OperationCompletion interface {
	applyToHTTPRequest(*http.Request) error
}

// OperationCompletionSuccessful is input for [NewCompletionHTTPRequest], used to deliver successful operation results.
type OperationCompletionSuccessful struct {
	// Header to send in the completion request.
	Header http.Header
	// Body to send in the completion HTTP request.
	// If it implements `io.Closer` it will automatically be closed by the client.
	Body io.Reader
	// OperationID is the unique ID for this operation. Used when a completion callback is received before a started response.
	OperationID string
	// StartTime is the time the operation started. Used when a completion callback is received before a started response.
	StartTime time.Time
	// StartLinks are used to link back to the operation when a completion callback is received before a started response.
	StartLinks []Link
}

// OperationCompletionSuccessfulOptions are options for [NewOperationCompletionSuccessful].
type OperationCompletionSuccessfulOptions struct {
	// Optional serializer for the result. Defaults to the SDK's default Serializer, which handles JSONables, byte
	// slices and nils.
	Serializer Serializer
	// OperationID is the unique ID for this operation. Used when a completion callback is received before a started response.
	OperationID string
	// StartTime is the time the operation started. Used when a completion callback is received before a started response.
	StartTime time.Time
	// StartLinks are used to link back to the operation when a completion callback is received before a started response.
	StartLinks []Link
}

// NewOperationCompletionSuccessful constructs an [OperationCompletionSuccessful] from a given result.
func NewOperationCompletionSuccessful(result any, options OperationCompletionSuccessfulOptions) (*OperationCompletionSuccessful, error) {
	if reader, ok := result.(*Reader); ok {
		return &OperationCompletionSuccessful{
			Header:      addContentHeaderToHTTPHeader(reader.Header, make(http.Header)),
			Body:        reader.ReadCloser,
			OperationID: options.OperationID,
			StartTime:   options.StartTime,
			StartLinks:  options.StartLinks,
		}, nil
	} else {
		content, ok := result.(*Content)
		if !ok {
			var err error
			serializer := options.Serializer
			if serializer == nil {
				serializer = defaultSerializer
			}
			content, err = serializer.Serialize(result)
			if err != nil {
				return nil, err
			}
		}
		header := http.Header{"Content-Length": []string{strconv.Itoa(len(content.Data))}}

		return &OperationCompletionSuccessful{
			Header:      addContentHeaderToHTTPHeader(content.Header, header),
			Body:        bytes.NewReader(content.Data),
			OperationID: options.OperationID,
			StartTime:   options.StartTime,
			StartLinks:  options.StartLinks,
		}, nil
	}
}

func (c *OperationCompletionSuccessful) applyToHTTPRequest(request *http.Request) error {
	if c.Header != nil {
		request.Header = c.Header.Clone()
	}
	request.Header.Set(headerOperationState, string(OperationStateSucceeded))
	if c.Header.Get(HeaderOperationID) == "" && c.OperationID != "" {
		request.Header.Set(HeaderOperationID, c.OperationID)
	}
	if c.Header.Get(headerOperationStartTime) == "" && !c.StartTime.IsZero() {
		request.Header.Set(headerOperationStartTime, c.StartTime.Format(http.TimeFormat))
	}
	if c.Header.Get(headerLink) == "" {
		if err := addLinksToHTTPHeader(c.StartLinks, request.Header); err != nil {
			return err
		}
	}

	if closer, ok := c.Body.(io.ReadCloser); ok {
		request.Body = closer
	} else {
		request.Body = io.NopCloser(c.Body)
	}
	return nil
}

// OperationCompletionUnsuccessful is input for [NewCompletionHTTPRequest], used to deliver unsuccessful operation
// results.
type OperationCompletionUnsuccessful struct {
	// Header to send in the completion request.
	Header http.Header
	// State of the operation, should be failed or canceled.
	State OperationState
	// OperationID is the unique ID for this operation. Used when a completion callback is received before a started response.
	OperationID string
	// StartTime is the time the operation started. Used when a completion callback is received before a started response.
	StartTime time.Time
	// StartLinks are used to link back to the operation when a completion callback is received before a started response.
	StartLinks []Link
	// Failure object to send with the completion.
	Failure *Failure
}

func (c *OperationCompletionUnsuccessful) applyToHTTPRequest(request *http.Request) error {
	if c.Header != nil {
		request.Header = c.Header.Clone()
	}
	request.Header.Set(headerOperationState, string(c.State))
	request.Header.Set("Content-Type", contentTypeJSON)
	if c.Header.Get(HeaderOperationID) == "" && c.OperationID != "" {
		request.Header.Set(HeaderOperationID, c.OperationID)
	}
	if c.Header.Get(headerOperationStartTime) == "" && !c.StartTime.IsZero() {
		request.Header.Set(headerOperationStartTime, c.StartTime.Format(http.TimeFormat))
	}
	if c.Header.Get(headerLink) == "" {
		if err := addLinksToHTTPHeader(c.StartLinks, request.Header); err != nil {
			return err
		}
	}

	b, err := json.Marshal(c.Failure)
	if err != nil {
		return err
	}

	request.Body = io.NopCloser(bytes.NewReader(b))
	return nil
}

// CompletionRequest is input for CompletionHandler.CompleteOperation.
type CompletionRequest struct {
	// The original HTTP request.
	HTTPRequest *http.Request
	// State of the operation.
	State OperationState
	// OperationID is the ID of the operation. Used when a completion callback is received before a started response.
	OperationID string
	// StartTime is the time the operation started. Used when a completion callback is received before a started response.
	StartTime time.Time
	// StartLinks are used to link back to the operation when a completion callback is received before a started response.
	StartLinks []Link
	// Parsed from request and set if State is failed or canceled.
	Failure *Failure
	// Extracted from request and set if State is succeeded.
	Result *LazyValue
}

// A CompletionHandler can receive operation completion requests as delivered via the callback URL provided in
// start-operation requests.
type CompletionHandler interface {
	CompleteOperation(context.Context, *CompletionRequest) error
}

// CompletionHandlerOptions are options for [NewCompletionHTTPHandler].
type CompletionHandlerOptions struct {
	// Handler for completion requests.
	Handler CompletionHandler
	// A stuctured logging handler.
	// Defaults to slog.Default().
	Logger *slog.Logger
	// A [Serializer] to customize handler serialization behavior.
	// By default the handler handles, JSONables, byte slices, and nil.
	Serializer Serializer
}

type completionHTTPHandler struct {
	baseHTTPHandler
	options CompletionHandlerOptions
}

func (h *completionHTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	completion := CompletionRequest{
		State:       OperationState(request.Header.Get(headerOperationState)),
		OperationID: request.Header.Get(HeaderOperationID),
		HTTPRequest: request,
	}
	if startTimeHeader := request.Header.Get(headerOperationStartTime); startTimeHeader != "" {
		var parseTimeErr error
		if completion.StartTime, parseTimeErr = http.ParseTime(startTimeHeader); parseTimeErr != nil {
			h.writeFailure(writer, HandlerErrorf(HandlerErrorTypeBadRequest, "failed to parse operation start time header"))
			return
		}
	}
	var decodeErr error
	if completion.StartLinks, decodeErr = getLinksFromHeader(request.Header); decodeErr != nil {
		h.writeFailure(writer, HandlerErrorf(HandlerErrorTypeBadRequest, "failed to decode links from request headers"))
		return
	}
	switch completion.State {
	case OperationStateFailed, OperationStateCanceled:
		if !isMediaTypeJSON(request.Header.Get("Content-Type")) {
			h.writeFailure(writer, HandlerErrorf(HandlerErrorTypeBadRequest, "invalid request content type: %q", request.Header.Get("Content-Type")))
			return
		}
		var failure Failure
		b, err := io.ReadAll(request.Body)
		if err != nil {
			h.writeFailure(writer, HandlerErrorf(HandlerErrorTypeBadRequest, "failed to read Failure from request body"))
			return
		}
		if err := json.Unmarshal(b, &failure); err != nil {
			h.writeFailure(writer, HandlerErrorf(HandlerErrorTypeBadRequest, "failed to read Failure from request body"))
			return
		}
		completion.Failure = &failure
	case OperationStateSucceeded:
		completion.Result = &LazyValue{
			serializer: h.options.Serializer,
			Reader: &Reader{
				request.Body,
				prefixStrippedHTTPHeaderToNexusHeader(request.Header, "content-"),
			},
		}
	default:
		h.writeFailure(writer, HandlerErrorf(HandlerErrorTypeBadRequest, "invalid request operation state: %q", completion.State))
		return
	}
	if err := h.options.Handler.CompleteOperation(ctx, &completion); err != nil {
		h.writeFailure(writer, err)
	}
}

// NewCompletionHTTPHandler constructs an [http.Handler] from given options for handling operation completion requests.
func NewCompletionHTTPHandler(options CompletionHandlerOptions) http.Handler {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Serializer == nil {
		options.Serializer = defaultSerializer
	}
	return &completionHTTPHandler{
		options: options,
		baseHTTPHandler: baseHTTPHandler{
			logger: options.Logger,
		},
	}
}
