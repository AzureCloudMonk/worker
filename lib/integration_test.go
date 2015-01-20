package lib

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/streadway/amqp"
	"golang.org/x/net/context"

	"github.com/travis-ci/worker/lib/backend"
)

func TestIntegration(t *testing.T) {
	amqpConn, err := amqp.Dial("amqp://")
	if err != nil {
		t.Fatalf("couldn't open AMQP connection: %v", err)
	}
	defer amqpConn.Close()

	amqpChan, err := amqpConn.Channel()
	if err != nil {
		t.Fatalf("couldn't open AMQP channel: %v", err)
	}
	defer amqpChan.Close()

	_, err = amqpChan.QueueDeclare("builds.test", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("couldn't declare builds queue: %v", err)
	}

	_, err = amqpChan.QueueDeclare("reporting.jobs.logs", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("couldn't declare logs queue: %v", err)
	}

	_, err = amqpChan.QueuePurge("builds.test", false)
	if err != nil {
		t.Fatalf("couldn't purge builds queue: %v", err)
	}

	_, err = amqpChan.QueuePurge("reporting.jobs.logs", false)
	if err != nil {
		t.Fatalf("couldn't purge logs queue: %v", err)
	}

	err = amqpChan.Publish("", "builds.test", false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Type:         "test",
		Body:         []byte(`{"type":"test","job":{"id":3,"number":"1.1","commit":"abcdef","commit_range":"abcde...abcdef","commit_message":"Hello world","branch":"master","ref":null,"state":"queued","secure_env_enabled":true,"pull_request":false},"source":{"id":2,"number":"1"},"repository":{"id":1,"slug":"hello/world","github_id":1234,"source_url":"git://github.com/hello/world.git","api_url":"https://api.github.com","last_build_id":2,"last_build_number":"1","last_build_started_at":null,"last_build_finished_at":null,"last_build_duration":null,"last_build_state":"created","description":"Hello world"},"config":{},"queue":"builds.test","uuid":"fake-uuid","ssh_key":null,"env_vars":[],"timeouts":{"hard_limit":null,"log_silence":null}}`),
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, client")
	}))
	defer ts.Close()

	ctx := context.Background()
	generator := NewBuildScriptGenerator(ts.URL)
	provider := backend.NewFakeProvider([]byte("Hello, testing log"))

	pool := &ProcessorPool{
		Context:   ctx,
		Conn:      amqpConn,
		Provider:  provider,
		Generator: generator,
	}

	go func() {
		time.Sleep(time.Second)
		pool.GracefulShutdown()
	}()

	pool.Run(1, "builds.test")

	deliveryChan, err := amqpChan.Consume("reporting.jobs.logs", "logs", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("error consuming to logs: %v")
	}

	select {
	case delivery := <-deliveryChan:
		var part logPart
		json.Unmarshal(delivery.Body, &part)
		expectedPart := logPart{
			JobID:   3,
			Content: "Hello, testing log",
			Number:  0,
			UUID:    "fake-uuid",
			Final:   false,
		}

		if !reflect.DeepEqual(part, expectedPart) {
			t.Errorf("logPart = %+v, expected %+v", part, expectedPart)
		}
	case <-time.After(500 * time.Microsecond):
		t.Errorf("expected a log part, but didn't get one within the timeout")
	}

	select {
	case delivery := <-deliveryChan:
		var part logPart
		json.Unmarshal(delivery.Body, &part)
		expectedPart := logPart{
			JobID:   3,
			Content: "",
			Number:  1,
			UUID:    "fake-uuid",
			Final:   true,
		}

		if !reflect.DeepEqual(part, expectedPart) {
			t.Errorf("logPart = %+v, expected %+v", part, expectedPart)
		}
	case <-time.After(500 * time.Microsecond):
		t.Errorf("expected a final log part, but didn't get one within the timeout")
	}

	amqpChan.Cancel("logs", false)
}
