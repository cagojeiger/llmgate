package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"llmgate/internal/catalog"
)

type AdapterFactory func(*catalog.Endpoint) (Provider, error)

type Router struct {
	byModel map[string]Provider
	log     *slog.Logger
}

func NewRouter(cat *catalog.Catalog, factories map[string]AdapterFactory, log *slog.Logger) (*Router, error) {
	if log == nil {
		log = slog.Default()
	}

	byEndpoint := make(map[string]Provider, len(cat.Endpoints))
	for _, ep := range cat.Endpoints {
		factory, ok := factories[ep.Protocol]
		if !ok {
			log.Warn("no adapter for protocol", slog.String("protocol", ep.Protocol), slog.String("endpoint", ep.Name))
			continue
		}
		p, err := factory(ep)
		if err != nil {
			log.Warn("adapter factory failed",
				slog.String("protocol", ep.Protocol),
				slog.String("endpoint", ep.Name),
				slog.String("err", err.Error()),
			)
			continue
		}
		byEndpoint[ep.Name] = p
	}

	byModel := make(map[string]Provider, len(cat.Models))
	for modelID, model := range cat.Models {
		p, ok := byEndpoint[model.Endpoint]
		if !ok {
			continue
		}
		byModel[strings.ToLower(modelID)] = p
	}
	if len(byModel) == 0 {
		return nil, errors.New("router: no models registered (check protocol factories)")
	}
	return &Router{byModel: byModel, log: log}, nil
}

func (r *Router) Name() string { return "router" }

func (r *Router) Complete(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, &Error{Kind: KindBadRequest, Message: "request is nil"}
	}
	p, ok := r.byModel[strings.ToLower(req.Model)]
	if !ok {
		return nil, &Error{Kind: KindBadRequest, Message: "unknown model: " + req.Model}
	}
	return p.Complete(ctx, req)
}

func (r *Router) CompleteStream(ctx context.Context, req *Request) (Stream, error) {
	if req == nil {
		return nil, &Error{Kind: KindBadRequest, Message: "request is nil"}
	}
	p, ok := r.byModel[strings.ToLower(req.Model)]
	if !ok {
		return nil, &Error{Kind: KindBadRequest, Message: "unknown model: " + req.Model}
	}
	return p.CompleteStream(ctx, req)
}
