package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/noahlsl/redis-selector/selector"
)

// Span is a minimal tracing surface. Swap this for OpenTelemetry later.
// Span 是最小追踪接口，后续可替换为 OpenTelemetry。
type Span interface {
	SetAttributes(kv ...any)
	RecordError(err error)
	End()
}

type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
}

type spanKey struct{}

func ContextWithSpan(ctx context.Context, span Span) context.Context {
	return context.WithValue(ctx, spanKey{}, span)
}

func SpanFromContext(ctx context.Context) Span {
	if span, ok := ctx.Value(spanKey{}).(Span); ok {
		return span
	}
	return nil
}

// LogTracer prints spans to the process log. Replace with otel.Tracer in production.
// LogTracer 把 span 打到进程日志；生产环境可替换成 otel.Tracer。
type LogTracer struct {
	seq atomic.Int64
}

func (t *LogTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	id := t.seq.Add(1)
	span := &logSpan{id: id, name: name, start: time.Now()}
	log.Printf("span start id=%d name=%s", id, name)
	return ContextWithSpan(ctx, span), span
}

type logSpan struct {
	id    int64
	name  string
	start time.Time
	attrs []string
	err   error
}

func (s *logSpan) SetAttributes(kv ...any) {
	for i := 0; i+1 < len(kv); i += 2 {
		s.attrs = append(s.attrs, fmt.Sprintf("%v=%v", kv[i], kv[i+1]))
	}
}

func (s *logSpan) RecordError(err error) { s.err = err }

func (s *logSpan) End() {
	if s.err != nil {
		log.Printf("span end id=%d name=%s cost=%s attrs=%v err=%v", s.id, s.name, time.Since(s.start), s.attrs, s.err)
		return
	}
	log.Printf("span end id=%d name=%s cost=%s attrs=%v", s.id, s.name, time.Since(s.start), s.attrs)
}

// TracingPicker opens one business-level span around Pick.
// TracingPicker 在一次业务 Pick 外开一个 span。
type TracingPicker struct {
	inner  selector.Picker
	tracer Tracer
}

func NewTracingPicker(inner selector.Picker, tracer Tracer) *TracingPicker {
	return &TracingPicker{inner: inner, tracer: tracer}
}

func (p *TracingPicker) Pick(ctx context.Context, selectorID string, opts ...selector.Option) (*selector.Result, error) {
	ctx, span := p.tracer.Start(ctx, "selector.Pick")
	defer span.End()
	span.SetAttributes("selector.id", selectorID)

	res, err := p.inner.Pick(ctx, selectorID, opts...)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(
		"selector.candidate_id", res.CandidateID,
		"selector.weight", res.Weight,
		"selector.version", res.Version,
	)
	return res, nil
}

// TracingDrawer opens one business-level span around Draw.
// TracingDrawer 在一次业务 Draw 外开一个 span。
type TracingDrawer struct {
	inner  selector.Drawer
	tracer Tracer
}

func NewTracingDrawer(inner selector.Drawer, tracer Tracer) *TracingDrawer {
	return &TracingDrawer{inner: inner, tracer: tracer}
}

func (d *TracingDrawer) Draw(ctx context.Context, selectorID string, opts ...selector.Option) (*selector.Result, error) {
	ctx, span := d.tracer.Start(ctx, "selector.Draw")
	defer span.End()
	span.SetAttributes("selector.id", selectorID)

	res, err := d.inner.Draw(ctx, selectorID, opts...)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(
		"selector.candidate_id", res.CandidateID,
		"selector.remain_stock", res.RemainStock,
		"selector.version", res.Version,
		"selector.replayed", res.Replayed,
	)
	return res, nil
}
