package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// MaxRetries is how many times a failing message is retried before it is dead-lettered.
const MaxRetries = 3

// RetryDelay is how long a failed message waits in the retry queue.
var RetryDelay = 5 * time.Second

const retryHeader = "x-retry-count"

// Broker wraps one RabbitMQ connection with a publishing channel.
type Broker struct {
	conn    *amqp.Connection
	pubCh   *amqp.Channel
	pubMu   sync.Mutex
	service string
}

// Connect dials RabbitMQ, retrying until it is reachable, and declares the exchange.
func Connect(url, service string) (*Broker, error) {
	var conn *amqp.Connection
	var err error
	for attempt := 1; attempt <= 30; attempt++ {
		conn, err = amqp.Dial(url)
		if err == nil {
			break
		}
		log.Printf("[broker] RabbitMQ not ready (attempt %d): %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect rabbitmq: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	// Publisher confirms: the broker tells us when a message is safely stored.
	if err := ch.Confirm(false); err != nil {
		return nil, err
	}
	if err := ch.ExchangeDeclare(Exchange, "topic", true, false, false, false, nil); err != nil {
		return nil, err
	}
	b := &Broker{conn: conn, pubCh: ch, service: service}

	// If the connection drops, exit so Docker restarts the service and it reconnects cleanly.
	go func() {
		if e := <-conn.NotifyClose(make(chan *amqp.Error, 1)); e != nil {
			log.Fatalf("[broker] connection to RabbitMQ lost: %v (exiting so the service restarts)", e)
		}
	}()
	log.Printf("[broker] connected to RabbitMQ as %s", service)
	return b, nil
}

// Close shuts the connection.
func (b *Broker) Close() { _ = b.conn.Close() }

// Publish sends a persistent event and waits for the broker's confirmation.
func (b *Broker) Publish(ctx context.Context, env Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b.pubMu.Lock()
	defer b.pubMu.Unlock()
	conf, err := b.pubCh.PublishWithDeferredConfirmWithContext(ctx, Exchange, env.EventType, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent, // survives a broker restart
		MessageId:    env.EventID,
		Timestamp:    env.OccurredAt,
		Type:         env.EventType,
		AppId:        b.service,
		Body:         body,
	})
	if err != nil {
		return err
	}
	ok, err := conf.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("broker did not confirm publish")
	}
	log.Printf("[publish] %s id=%s", env.EventType, env.EventID)
	return nil
}

// Emit builds and publishes an event in one call.
func (b *Broker) Emit(ctx context.Context, eventType string, payload any) (Envelope, error) {
	env, err := New(eventType, b.service, payload)
	if err != nil {
		return env, err
	}
	return env, b.Publish(ctx, env)
}

// Handler processes one event. Returning an error triggers a retry.
type Handler func(ctx context.Context, env Envelope) error

// Subscribe declares this service's durable queue (with retry and dead-letter
// queues), binds it to the given event types and starts consuming.
//
// Topology for queue "fine.events":
//
//	library.events --(routing keys)--> fine.events --(handler error)--> fine.events.retry (TTL)
//	                                        ^                               |
//	                                        +-------- after delay ----------+
//	after MaxRetries failures, or an unreadable message: --> fine.events.dlq
func (b *Broker) Subscribe(queue string, eventTypes []string, handle Handler) error {
	ch, err := b.conn.Channel()
	if err != nil {
		return err
	}
	retryQ, dlq := queue+".retry", queue+".dlq"

	if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
		return err
	}
	// Messages rejected from the main queue go straight to the DLQ.
	if _, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": dlq,
	}); err != nil {
		return err
	}
	// Retry queue: messages wait for the TTL, then dead-letter back into the main queue.
	if _, err := ch.QueueDeclare(retryQ, true, false, false, false, amqp.Table{
		"x-message-ttl":             int32(RetryDelay / time.Millisecond),
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": queue,
	}); err != nil {
		return err
	}
	for _, et := range eventTypes {
		if err := ch.QueueBind(queue, et, Exchange, false, nil); err != nil {
			return err
		}
	}
	// Only hand us 10 unacknowledged messages at a time.
	if err := ch.Qos(10, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queue, b.service, false /* manual ack */, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Printf("[consumer] listening on %s for %v", queue, eventTypes)

	go func() {
		for d := range deliveries {
			b.process(ch, d, queue, retryQ, dlq, handle)
		}
		log.Fatalf("[consumer] delivery channel for %s closed", queue)
	}()
	return nil
}

func (b *Broker) process(ch *amqp.Channel, d amqp.Delivery, queue, retryQ, dlq string, handle Handler) {
	var env Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil || env.EventID == "" {
		log.Printf("[consumer] %s: unreadable message, sending to %s: %v", queue, dlq, err)
		_ = d.Nack(false, false) // dead-letters to the DLQ
		return
	}
	retries := retryCount(d.Headers)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := handle(ctx, env)
	cancel()
	if err == nil {
		_ = d.Ack(false)
		return
	}

	if retries >= MaxRetries {
		log.Printf("[consumer] %s id=%s failed %d times, moving to DLQ: %v", env.EventType, env.EventID, retries+1, err)
		if !b.forward(ch, d, dlq, retries, err) {
			return
		}
	} else {
		log.Printf("[consumer] %s id=%s failed (attempt %d/%d), retrying in %s: %v",
			env.EventType, env.EventID, retries+1, MaxRetries+1, RetryDelay, err)
		if !b.forward(ch, d, retryQ, retries+1, err) {
			return
		}
	}
	// Ack the original only after the copy is safely in the retry/DLQ queue.
	_ = d.Ack(false)
}

func (b *Broker) forward(ch *amqp.Channel, d amqp.Delivery, target string, retries int, cause error) bool {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[retryHeader] = int32(retries)
	headers["x-last-error"] = cause.Error()
	if err := ch.PublishWithContext(context.Background(), "", target, false, false, amqp.Publishing{
		ContentType:  d.ContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    d.MessageId,
		Type:         d.Type,
		Headers:      headers,
		Body:         d.Body,
	}); err != nil {
		// Could not park it: put it back on the queue rather than lose it.
		log.Printf("[consumer] could not forward to %s, requeueing: %v", target, err)
		_ = d.Nack(false, true)
		return false
	}
	return true
}

func retryCount(h amqp.Table) int {
	switch v := h[retryHeader].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	}
	return 0
}
