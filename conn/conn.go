package conn

import (
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/10gen/mongo-go-driver/desc"
	"github.com/10gen/mongo-go-driver/internal"
	"github.com/10gen/mongo-go-driver/msg"

	"io"

	"gopkg.in/mgo.v2/bson"
)

var globalClientConnectionID int32

func nextClientConnectionID() int32 {
	return atomic.AddInt32(&globalClientConnectionID, 1)
}

// Dialer dials a connection.
type Dialer func(desc.Endpoint, ...Option) (ConnectionCloser, error)

// Dial opens a connection to a server.
func Dial(endpoint desc.Endpoint, opts ...Option) (ConnectionCloser, error) {

	cfg := newConfig(opts...)

	transport, err := cfg.dialer(endpoint)
	if err != nil {
		return nil, err
	}

	c := &transportConnection{
		id:        fmt.Sprintf("%s[-%d]", endpoint, nextClientConnectionID()),
		codec:     cfg.codec,
		ep:        endpoint,
		transport: transport,
	}

	err = c.initialize(cfg.appName)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// Connection is responsible for reading and writing messages.
type Connection interface {
	// Desc gets a description of the connection.
	Desc() *desc.Connection
	// Read reads a message from the connection for the
	// specified requestID.
	Read() (msg.Response, error)
	// Write writes a number of messages to the connection.
	Write(...msg.Request) error
}

// ConnectionCloser is a Connection that can be closed.
type ConnectionCloser interface {
	Connection

	// Closes the connection.
	Close() error
}

// ConnectionError represents an error that in the connection package.
type ConnectionError struct {
	ConnectionID string

	message string
	inner   error
}

// Message gets the basic error message.
func (e *ConnectionError) Message() string {
	return e.message
}

// Error gets a rolled-up error message.
func (e *ConnectionError) Error() string {
	return internal.RolledUpErrorMessage(e)
}

// Inner gets the inner error if one exists.
func (e *ConnectionError) Inner() error {
	return e.inner
}

type transportConnection struct {
	// if id is negative, it's the client identifier; otherwise it's the same
	// as the id the server is using.
	id        string
	codec     msg.Codec
	desc      *desc.Connection
	ep        desc.Endpoint
	transport io.ReadWriteCloser
}

func (c *transportConnection) Close() error {
	err := c.transport.Close()
	if err != nil {
		return c.wrapError(err, "failed closing")
	}

	return nil
}

func (c *transportConnection) Desc() *desc.Connection {
	return c.desc
}

func (c *transportConnection) Read() (msg.Response, error) {
	message, err := c.codec.Decode(c.transport)
	if err != nil {
		return nil, c.wrapError(err, "failed reading")
	}

	resp, ok := message.(msg.Response)
	if !ok {
		return nil, c.wrapError(err, "failed reading: invalid message type received")
	}

	return resp, nil
}

func (c *transportConnection) String() string {
	return c.id
}

func (c *transportConnection) Write(requests ...msg.Request) error {
	var messages []msg.Message
	for _, message := range requests {
		messages = append(messages, message)
	}

	err := c.codec.Encode(c.transport, messages...)
	if err != nil {
		return c.wrapError(err, "failed writing")
	}
	return nil
}

func (c *transportConnection) initialize(appName string) error {

	isMasterResult, buildInfoResult, err := describeServer(c, createClientDoc(appName))
	if err != nil {
		return err
	}

	getLastErrorReq := msg.NewCommand(
		msg.NextRequestID(),
		"admin",
		true,
		bson.D{{"getLastError", 1}},
	)

	c.desc = &desc.Connection{
		GitVersion:          buildInfoResult.GitVersion,
		Version:             desc.NewVersionWithDesc(buildInfoResult.Version, buildInfoResult.VersionArray...),
		MaxBSONObjectSize:   isMasterResult.MaxBSONObjectSize,
		MaxMessageSizeBytes: isMasterResult.MaxMessageSizeBytes,
		MaxWriteBatchSize:   isMasterResult.MaxWriteBatchSize,
		ReadOnly:            isMasterResult.ReadOnly,
		WireVersion:         desc.Range{Min: isMasterResult.MinWireVersion, Max: isMasterResult.MaxWireVersion},
	}

	var getLastErrorResult internal.GetLastErrorResult
	err = ExecuteCommand(c, getLastErrorReq, &getLastErrorResult)
	// NOTE: we don't care about this result. If it fails, it doesn't
	// harm us in any way other than not being able to correlate
	// our logs with the server's logs.
	if err == nil {
		c.id = fmt.Sprintf("%s[%d]", c.ep, getLastErrorResult.ConnectionID)
	}

	return nil
}

func (c *transportConnection) wrapError(inner error, message string) error {
	return &ConnectionError{
		c.id,
		fmt.Sprintf("connection(%s) error: %s", c.id, message),
		inner,
	}
}

func createClientDoc(appName string) bson.M {
	clientDoc := bson.M{
		"driver": bson.M{
			"name":    "mongo-go-driver",
			"version": internal.Version,
		},
		"os": bson.M{
			"type":         "unknown",
			"name":         runtime.GOOS,
			"architecture": runtime.GOARCH,
			"version":      "unknown",
		},
		"platform": nil,
	}
	if appName != "" {
		clientDoc["application"] = bson.M{"name": appName}
	}

	return clientDoc
}

func describeServer(c Connection, clientDoc bson.M) (*internal.IsMasterResult, *internal.BuildInfoResult, error) {
	isMasterCmd := bson.D{{Name: "ismaster", Value: 1}}
	if clientDoc != nil {
		isMasterCmd = append(isMasterCmd, bson.DocElem{
			Name:  "client",
			Value: clientDoc,
		})
	}

	isMasterReq := msg.NewCommand(
		msg.NextRequestID(),
		"admin",
		true,
		isMasterCmd,
	)
	buildInfoReq := msg.NewCommand(
		msg.NextRequestID(),
		"admin",
		true,
		bson.D{{Name: "buildInfo", Value: 1}},
	)

	var isMasterResult internal.IsMasterResult
	var buildInfoResult internal.BuildInfoResult
	err := ExecuteCommands(c, []msg.Request{isMasterReq, buildInfoReq}, []interface{}{&isMasterResult, &buildInfoResult})
	if err != nil {
		return nil, nil, err
	}

	return &isMasterResult, &buildInfoResult, nil
}