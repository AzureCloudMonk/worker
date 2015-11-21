package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pborman/uuid"
	"github.com/streadway/amqp"
	workerctx "github.com/travis-ci/worker/context"
	"golang.org/x/net/context"
)

func setupConn(t *testing.T) (*amqp.Connection, *amqp.Channel) {
	amqpConn, err := amqp.Dial(os.Getenv("AMQP_URI"))
	if err != nil {
		t.Fatal(err)
	}

	amqpChan, err := amqpConn.Channel()
	if err != nil {
		t.Fatal(err)
	}

	_, err = amqpChan.QueueDeclare("reporting.jobs.logs", true, false, false, false, nil)
	if err != nil {
		t.Error(err)
	}

	_, err = amqpChan.QueuePurge("reporting.jobs.logs", false)
	if err != nil {
		t.Error(err)
	}

	return amqpConn, amqpChan
}

func TestAMQPLogWriterWrite(t *testing.T) {
	amqpConn, amqpChan := setupConn(t)
	defer amqpConn.Close()
	defer amqpChan.Close()

	uuid := uuid.NewRandom()
	ctx := workerctx.FromUUID(context.TODO(), uuid.String())

	logWriter, err := newAMQPLogWriter(ctx, amqpConn, 4)
	if err != nil {
		t.Fatal(err)
	}
	logWriter.SetMaxLogLength(1000)
	logWriter.SetTimeout(time.Second)

	_, err = fmt.Fprintf(logWriter, "Hello, ")
	if err != nil {
		t.Error(err)
	}
	_, err = fmt.Fprintf(logWriter, "world!")
	if err != nil {
		t.Error(err)
	}

	// Close the log writer to force it to flush out the buffer
	err = logWriter.Close()
	if err != nil {
		t.Error(err)
	}

	delivery, ok, err := amqpChan.Get("reporting.jobs.logs", true)
	if err != nil {
		t.Error(err)
	}
	if !ok {
		t.Error("expected log message, but there was none")
	}

	var lp amqpLogPart

	err = json.Unmarshal(delivery.Body, &lp)
	if err != nil {
		t.Error(err)
	}

	expected := amqpLogPart{
		JobID:   4,
		Content: "Hello, world!",
		Number:  0,
		UUID:    uuid.String(),
		Final:   false,
	}

	if expected != lp {
		t.Errorf("log part is %#v, expected %#v", lp, expected)
	}
}

func TestAMQPLogWriterClose(t *testing.T) {
	amqpConn, amqpChan := setupConn(t)
	defer amqpConn.Close()
	defer amqpChan.Close()

	uuid := uuid.NewRandom()
	ctx := workerctx.FromUUID(context.TODO(), uuid.String())

	logWriter, err := newAMQPLogWriter(ctx, amqpConn, 4)
	if err != nil {
		t.Fatal(err)
	}
	logWriter.SetMaxLogLength(1000)
	logWriter.SetTimeout(time.Second)

	// Close the log writer to force it to flush out the buffer
	err = logWriter.Close()
	if err != nil {
		t.Error(err)
	}

	delivery, ok, err := amqpChan.Get("reporting.jobs.logs", true)
	if err != nil {
		t.Error(err)
	}
	if !ok {
		t.Error("expected log message, but there was none")
	}

	var lp amqpLogPart

	err = json.Unmarshal(delivery.Body, &lp)
	if err != nil {
		t.Error(err)
	}

	expected := amqpLogPart{
		JobID:   4,
		Content: "",
		Number:  0,
		UUID:    uuid.String(),
		Final:   true,
	}

	if expected != lp {
		t.Errorf("log part is %#v, expected %#v", lp, expected)
	}
}

func TestAMQPMaxLogLength(t *testing.T) {
	amqpConn, amqpChan := setupConn(t)
	defer amqpConn.Close()
	defer amqpChan.Close()

	uuid := uuid.NewRandom()
	ctx := workerctx.FromUUID(context.TODO(), uuid.String())

	logWriter, err := newAMQPLogWriter(ctx, amqpConn, 4)
	if err != nil {
		t.Fatal(err)
	}
	logWriter.SetMaxLogLength(4)
	logWriter.SetTimeout(time.Second)

	_, err = fmt.Fprintf(logWriter, "1234")
	if err != nil {
		t.Error(err)
	}
	_, err = fmt.Fprintf(logWriter, "5")
	if err == nil {
		t.Error("expected error, but got nil")
	}
}
