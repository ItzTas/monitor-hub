package consumerapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/ItzTas/monitor-hub/internal/api"
	"github.com/ItzTas/monitor-hub/internal/consumers"
	"github.com/ItzTas/monitor-hub/internal/stream"
)

const (
	maxStreams   = 16
	retryAfter   = "30"
	invalidToken = "missing or invalid consumer token"
)

type Deps struct {
	Consumers *consumers.Service
	Stream    *stream.Service
	Log       *slog.Logger
}

type AckInput struct {
	Body struct {
		Seq int64 `json:"seq" minimum:"0" doc:"The seq of the last event the consumer handled, or the seq of caught_up."`
	}
}

type AckOutput struct {
	Body struct {
		Position int64 `json:"position" doc:"The consumer's position after the acknowledgement; it never moves back."`
	}
}

type caller struct {
	consumer  consumers.Consumer
	tokenHash string
}

type callerKey struct{}

type handler struct {
	consumers *consumers.Service
	streams   *stream.Service
	log       *slog.Logger
}

func Register(mux *http.ServeMux, deps Deps) {
	protocol := humago.New(mux, api.HumaConfig(deps.Log,
		"The consumer protocol, version 1: the hub's notifications as a stream, and the acknowledgement of what a consumer handled. Every route needs the header Authorization: Bearer <consumer token>.",
		"The consumer's own token, shown once by monitor-hub consumers add or rotate.",
	))
	protocol.UseMiddleware(authenticate(protocol, deps.Consumers))
	h := &handler{consumers: deps.Consumers, streams: deps.Stream, log: deps.Log}
	sse.Register(protocol, huma.Operation{
		OperationID: "consumer-stream",
		Method:      http.MethodGet,
		Path:        "/consumer/v1/stream",
		Summary:     "Stream the notifications",
		Description: "Sends every event after the consumer's position, then caught_up, then each new event as the hub stores it, with a ping comment every 25 seconds. Last-Event-ID is ignored. A newer stream of the same consumer replaces this one.",
		Tags:        []string{"consumers"},
		Errors:      []int{http.StatusUnauthorized, http.StatusServiceUnavailable},
		Middlewares: huma.Middlewares{onlyGet(protocol), limit(protocol, make(chan struct{}, maxStreams))},
	}, map[string]any{
		"notification": stream.Notification{},
		"caught_up":    stream.CaughtUp{},
		"replaced":     stream.Replaced{},
	}, h.stream)
	huma.Register(protocol, huma.Operation{
		OperationID: "consumer-ack",
		Method:      http.MethodPost,
		Path:        "/consumer/v1/ack",
		Summary:     "Acknowledge what the consumer handled",
		Description: "Moves the consumer's position forward to seq; a lower seq leaves it where it is. The next stream starts after the position.",
		Tags:        []string{"consumers"},
		Errors:      []int{http.StatusUnauthorized, http.StatusUnprocessableEntity},
	}, h.ack)
}

func authenticate(protocol huma.API, service *consumers.Service) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		scheme, token, _ := strings.Cut(ctx.Header("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") {
			unauthorized(protocol, ctx)
			return
		}
		consumer, err := service.Authenticate(ctx.Context(), token)
		if errors.Is(err, consumers.ErrNotFound) {
			unauthorized(protocol, ctx)
			return
		}
		if err != nil {
			_ = huma.WriteErr(protocol, ctx, http.StatusInternalServerError, "internal error", err)
			return
		}
		next(huma.WithValue(ctx, callerKey{}, caller{consumer: consumer, tokenHash: consumers.HashToken(token)}))
	}
}

func unauthorized(protocol huma.API, ctx huma.Context) {
	ctx.SetHeader("WWW-Authenticate", "Bearer")
	_ = huma.WriteErr(protocol, ctx, http.StatusUnauthorized, invalidToken)
}

func onlyGet(protocol huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if ctx.Method() == http.MethodGet {
			next(ctx)
			return
		}
		ctx.SetHeader("Allow", http.MethodGet)
		_ = huma.WriteErr(protocol, ctx, http.StatusMethodNotAllowed, "the stream is read with GET")
	}
}

func limit(protocol huma.API, slots chan struct{}) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		select {
		case slots <- struct{}{}:
		default:
			ctx.SetHeader("Retry-After", retryAfter)
			_ = huma.WriteErr(protocol, ctx, http.StatusServiceUnavailable, "too many open consumer streams")
			return
		}
		defer func() { <-slots }()
		next(ctx)
	}
}

func (h *handler) stream(ctx context.Context, _ *struct{}, send sse.Sender) {
	c := ctx.Value(callerKey{}).(caller)
	name := c.consumer.Name
	err := h.streams.Stream(ctx, stream.Client{
		Name:     name,
		Position: c.consumer.Position,
		Check:    func(ctx context.Context) (bool, error) { return h.consumers.Touch(ctx, name, c.tokenHash) },
		Clamp:    func(ctx context.Context, latest int64) error { return h.consumers.ClampPosition(ctx, name, latest) },
	}, send)
	if err == nil || ctx.Err() != nil {
		return
	}
	h.log.Warn("consumer stream ended with an error", "consumer", name, "error", err)
}

func (h *handler) ack(ctx context.Context, input *AckInput) (*AckOutput, error) {
	c := ctx.Value(callerKey{}).(caller)
	position, err := h.consumers.Ack(ctx, c.consumer.Name, c.tokenHash, input.Body.Seq)
	if errors.Is(err, consumers.ErrSeqOutOfRange) {
		return nil, huma.Error422UnprocessableEntity("seq is beyond the newest event")
	}
	if errors.Is(err, consumers.ErrNotFound) {
		return nil, huma.ErrorWithHeaders(huma.Error401Unauthorized(invalidToken), http.Header{"WWW-Authenticate": {"Bearer"}})
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error", err)
	}
	output := &AckOutput{}
	output.Body.Position = position
	return output, nil
}
