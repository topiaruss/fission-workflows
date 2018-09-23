package fes

import (
	"fmt"

	"github.com/fission/fission-workflows/pkg/util/pubsub"
	"github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
)

// Entity is a entity that can be updated
type Entity interface {
	// Entity-specific
	ApplyEvent(event *Event) error

	// Aggregate provides type information about the entity, such as the aggregate id and the aggregate type.
	//
	// This is implemented by BaseEntity
	Aggregate() Aggregate

	// UpdateState mutates the current entity to the provided entityFactory state
	//
	// This is implemented by BaseEntity, can be overridden for performance approach
	UpdateState(targetState Entity) error

	// Copy copies the actual wrapped object. This is useful to get a snapshot of the state.
	CopyEntity() Entity
}

type EventAppender interface {
	Append(event *Event) error
}

// Backend is a persistent store for events
type Backend interface {
	EventAppender

	// Get fetches all events that belong to a specific aggregate
	Get(aggregate Aggregate) ([]*Event, error)
	List(matcher StringMatcher) ([]Aggregate, error)
}

// Projector projects events onto an entity
type Projector interface {
	Project(target Entity, events ...*Event) error
}

type CacheReader interface {
	Get(entity Entity) error
	List() []Aggregate
	GetAggregate(a Aggregate) (Entity, error)
}

type CacheWriter interface {
	Put(entity Entity) error
	Invalidate(entity *Aggregate)
}

type CacheReaderWriter interface {
	CacheReader
	CacheWriter
}

type StringMatcher func(target string) bool

var (
	DefaultNotificationBuffer = 64
)

type Notification struct {
	*pubsub.EmptyMsg
	Payload   Entity
	EventType string
	SpanCtx   opentracing.SpanContext
}

func newNotification(entity Entity, event *Event) *Notification {
	var spanCtx opentracing.SpanContext
	if event.Metadata != nil {
		sctx, err := opentracing.GlobalTracer().Extract(opentracing.TextMap, opentracing.TextMapCarrier(event.Metadata))
		if err != nil && err != opentracing.ErrSpanContextNotFound {
			logrus.Warnf("failed to extract opentracing tracer from event: %v", err)
		}
		spanCtx = sctx
	}
	return &Notification{
		EmptyMsg:  pubsub.NewEmptyMsg(event.Labels(), event.CreatedAt()),
		Payload:   entity,
		EventType: event.Type,
		SpanCtx:   spanCtx,
	}
}

// EventStoreErr is the base error type returned by functions in the fes package.
//
// Based on the context it provides additional information, such as the aggregate and event related to the error.
type EventStoreErr struct {
	// S is the description of the error. (required)
	S string

	// K is the aggregate related to the error. (optional)
	K *Aggregate

	// E is the event related to the error. (optional)
	E *Event

	// C is the underlying cause of the error. (optional)
	C error
}

func (err *EventStoreErr) WithAggregate(aggregate *Aggregate) *EventStoreErr {
	err.K = aggregate
	return err
}

func (err *EventStoreErr) WithEntity(entity Entity) *EventStoreErr {
	key := entity.Aggregate()
	err.K = &key
	return err
}

func (err *EventStoreErr) WithEvent(event *Event) *EventStoreErr {
	err.E = event
	if err.K == nil {
		return err.WithAggregate(event.Aggregate)
	}
	return err
}

func (err *EventStoreErr) Is(other error) bool {
	if err != nil && other == nil {
		return false
	}
	esErr, ok := other.(*EventStoreErr)
	if !ok {
		return false
	}
	return esErr.S == err.S
}

func (err *EventStoreErr) WithError(cause error) *EventStoreErr {
	err.C = cause
	return err
}

func (err *EventStoreErr) Error() string {
	if err.K == nil {
		return err.S
	} else {
		return fmt.Sprintf("%v: %s", err.K.Format(), err.S)
	}
}

var (
	ErrInvalidAggregate       = &EventStoreErr{S: "invalid aggregate"}
	ErrInvalidEvent           = &EventStoreErr{S: "invalid event"}
	ErrInvalidEntity          = &EventStoreErr{S: "invalid entity"}
	ErrEventStoreOverflow     = &EventStoreErr{S: "event store out of space"}
	ErrUnsupportedEntityEvent = &EventStoreErr{S: "event not supported"}
	ErrCorruptedEventPayload  = &EventStoreErr{S: "failed to parse event payload"}
	ErrEntityNotFound         = &EventStoreErr{S: "entity not found"}
)
