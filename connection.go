package dbus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
)

const defaultSystemBusAddress = "unix:path=/var/run/dbus/system_bus_socket"

// Connection represents a connection to a message bus (usually, the system or
// session bus).
//
// Multiple goroutines may invoke methods on a connection simultaneously.
type Connection struct {
	transport       net.Conn
	uuid            string
	names           []string
	namesLck        sync.RWMutex
	serial          chan uint32
	serialUsed      chan uint32
	replies         map[uint32]chan *Reply
	repliesLck      sync.RWMutex
	handlers        map[ObjectPath]map[string]interface{}
	handlersLck     sync.RWMutex
	out             chan *Message
	signals         chan Signal
	signalsLck      sync.Mutex
	eavesdropped    chan *Message
	eavesdroppedLck sync.Mutex
	busObj          *Object
}

// ConnectSessionBus connects to the session message bus and returns the
// connection or any error that occured.
func ConnectSessionBus() (*Connection, error) {
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address != "" && address != "autolaunch:" {
		return NewConnection(address)
	}
	cmd := exec.Command("dbus-launch")
	b, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	i := bytes.IndexByte(b, '=')
	j := bytes.IndexByte(b, '\n')
	if i == -1 || j == -1 {
		return nil, errors.New("couldn't determine address of the session bus")
	}
	return NewConnection(string(b[i+1 : j]))
}

// ConnectSystemBus connects to the system message bus and returns the
// connection or any error that occured.
func ConnectSystemBus() (*Connection, error) {
	address := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
	if address != "" {
		return NewConnection(address)
	}
	return NewConnection(defaultSystemBusAddress)
}

// NewConnection establishes a new connection to the message bus specified by
// address.
func NewConnection(address string) (*Connection, error) {
	var err error
	conn := new(Connection)
	if strings.HasPrefix(address, "unix") {
		abstract := getKey(address, "abstract")
		path := getKey(address, "path")
		switch {
		case abstract == "" && path == "":
			return nil, errors.New("bad address: neither path nor abstract set")
		case abstract != "" && path == "":
			conn.transport, err = net.Dial("unix", "@"+abstract)
			if err != nil {
				return nil, err
			}
		case abstract == "" && path != "":
			conn.transport, err = net.Dial("unix", path)
			if err != nil {
				return nil, err
			}
		case abstract != "" && path != "":
			return nil, errors.New("bad address: both path and abstract set")
		}
	} else {
		return nil, errors.New("bad address: invalid or unsupported transport")
	}
	if err = conn.auth(); err != nil {
		conn.transport.Close()
		return nil, err
	}
	conn.replies = make(map[uint32]chan *Reply)
	conn.out = make(chan *Message, 10)
	conn.handlers = make(map[ObjectPath]map[string]interface{})
	conn.serial = make(chan uint32)
	conn.serialUsed = make(chan uint32)
	conn.busObj = conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus")
	go conn.inWorker()
	go conn.outWorker()
	go conn.serials()
	if err = conn.hello(); err != nil {
		conn.transport.Close()
		return nil, err
	}
	return conn, nil
}

// BusObject returns the message bus object (i.e.
// conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus")).
func (conn *Connection) BusObject() *Object {
	return conn.busObj
}

// Close closes the underlying transport of the connection and stops all
// related goroutines.
func (conn *Connection) Close() error {
	close(conn.out)
	conn.signalsLck.Lock()
	if conn.signals != nil {
		close(conn.signals)
	}
	conn.signalsLck.Unlock()
	conn.eavesdroppedLck.Lock()
	if conn.eavesdropped != nil {
		close(conn.eavesdropped)
	}
	conn.eavesdroppedLck.Unlock()
	return conn.transport.Close()
}

// Eavesdrop changes the channel to which all messages are sent whose
// destination field is not one of the known names of this connection and which
// are not signals. The caller has to make sure that c is sufficiently buffered;
// if a message arrives when a write to c is not possible, the message is
// discarded.
//
// The channel can be reset by passing nil.
//
// If the connection is closed by the server or a call to Close, the channel is
// also closed.
func (conn *Connection) Eavesdrop(c chan *Message) {
	conn.eavesdroppedLck.Lock()
	conn.eavesdropped = c
	conn.eavesdroppedLck.Unlock()
}

// hello sends the initial org.freedesktop.DBus.Hello call.
func (conn *Connection) hello() error {
	var s string
	err := conn.busObj.Call("org.freedesktop.DBus.Hello", 0).Store(&s)
	if err != nil {
		return err
	}
	conn.namesLck.Lock()
	conn.names = make([]string, 1)
	conn.names[0] = s
	conn.namesLck.Unlock()
	return nil
}

// inWorker runs in an own goroutine, reading incoming messages from the
// transport and dispatching them appropiately.
func (conn *Connection) inWorker() {
	for {
		msg, err := conn.readMessage()
		if err == nil {
			dest, _ := msg.Headers[FieldDestination].value.(string)
			found := false
			conn.namesLck.RLock()
			if len(conn.names) == 0 {
				found = true
			}
			for _, v := range conn.names {
				if dest == v {
					found = true
					break
				}
			}
			conn.namesLck.RUnlock()
			conn.eavesdroppedLck.Lock()
			if !found && (msg.Type != TypeSignal || conn.eavesdropped != nil) {
				select {
				case conn.eavesdropped <- msg:
				default:
				}
				conn.eavesdroppedLck.Unlock()
				continue
			}
			conn.eavesdroppedLck.Unlock()
			switch msg.Type {
			case TypeMethodReply, TypeError:
				var reply *Reply
				serial := msg.Headers[FieldReplySerial].value.(uint32)
				if reply == nil {
					if msg.Type == TypeError {
						name, _ := msg.Headers[FieldErrorName].value.(string)
						reply = &Reply{nil, Error{name, msg.Body}}
					} else {
						reply = &Reply{msg.Body, nil}
					}
				}
				conn.repliesLck.Lock()
				if c, ok := conn.replies[serial]; ok {
					c <- reply
					conn.serialUsed <- serial
					delete(conn.replies, serial)
				}
				conn.repliesLck.Unlock()
			case TypeSignal:
				var signal Signal
				signal.Name = msg.Headers[FieldMember].value.(string)
				signal.Body = msg.Body
				// don't block trying to send a signal
				conn.signalsLck.Lock()
				select {
				case conn.signals <- signal:
				default:
				}
				conn.signalsLck.Unlock()
			case TypeMethodCall:
				go conn.handleCall(msg)
			}
		} else if _, ok := err.(InvalidMessageError); !ok {
			// Some read error occured (usually EOF); we can't really do
			// anything but to shut down all stuff and returns errors to all
			// pending replies.
			conn.Close()
			conn.repliesLck.RLock()
			for _, v := range conn.replies {
				v <- &Reply{nil, err}
			}
			conn.repliesLck.RUnlock()
			return
		}
		// invalid messages are ignored
	}
}

// outWorker runs in an own goroutine, encoding and sending messages that are
// sent to conn.out.
func (conn *Connection) outWorker() {
	for msg := range conn.out {
		err := msg.EncodeTo(conn.transport)
		conn.repliesLck.RLock()
		if err != nil {
			if conn.replies[msg.Serial] != nil {
				conn.replies[msg.Serial] <- &Reply{nil, err}
			}
			conn.serialUsed <- msg.Serial
		} else if msg.Type != TypeMethodCall {
			conn.serialUsed <- msg.Serial
		}
		conn.repliesLck.RUnlock()
	}
}

// readMessage reads and decodes a single message from the transport.
func (conn *Connection) readMessage() (*Message, error) {
	// read the first 16 bytes, from which we can figure out the length of the
	// rest of the message
	var header [16]byte
	if _, err := io.ReadFull(conn.transport, header[:]); err != nil {
		return nil, err
	}
	var order binary.ByteOrder
	switch header[0] {
	case 'l':
		order = binary.LittleEndian
	case 'B':
		order = binary.BigEndian
	default:
		return nil, InvalidMessageError("invalid byte order")
	}
	// header[4:8] -> length of message body, header[12:16] -> length of header
	// fields (without alignment)
	var blen, hlen uint32
	binary.Read(bytes.NewBuffer(header[4:8]), order, &blen)
	binary.Read(bytes.NewBuffer(header[12:16]), order, &hlen)
	if hlen%8 != 0 {
		hlen += 8 - (hlen % 8)
	}
	rest := make([]byte, int(blen+hlen))
	if _, err := io.ReadFull(conn.transport, rest); err != nil {
		return nil, err
	}
	all := make([]byte, 16+len(rest))
	copy(all, header[:])
	copy(all[16:], rest)
	return DecodeMessage(bytes.NewBuffer(all))
}

// sendError creates an error message corresponding to the parameters and sends
// it to conn.out.
func (conn *Connection) sendError(e Error, dest string, serial uint32) {
	msg := new(Message)
	msg.Order = binary.LittleEndian
	msg.Type = TypeError
	msg.Serial = <-conn.serial
	msg.Headers = make(map[HeaderField]Variant)
	msg.Headers[FieldDestination] = MakeVariant(dest)
	msg.Headers[FieldErrorName] = MakeVariant(e.Name)
	msg.Headers[FieldReplySerial] = MakeVariant(serial)
	msg.Body = e.Body
	if len(e.Body) > 0 {
		msg.Headers[FieldSignature] = MakeVariant(GetSignature(e.Body...))
	}
	conn.out <- msg
}

// sendReply creates a method reply message corresponding to the parameters and
// sends it to conn.out.
func (conn *Connection) sendReply(dest string, serial uint32, values ...interface{}) {
	msg := new(Message)
	msg.Order = binary.LittleEndian
	msg.Type = TypeMethodReply
	msg.Serial = <-conn.serial
	msg.Headers = make(map[HeaderField]Variant)
	msg.Headers[FieldDestination] = MakeVariant(dest)
	msg.Headers[FieldReplySerial] = MakeVariant(serial)
	msg.Body = values
	if len(values) > 0 {
		msg.Headers[FieldSignature] = MakeVariant(GetSignature(values...))
	}
	conn.out <- msg
}

// serials runs in an own goroutine, constantly sending serials on conn.serial
// and reading serials that are ready for "recycling" from conn.serialUsed.
func (conn *Connection) serials() {
	s := uint32(1)
	used := make(map[uint32]bool)
	used[0] = true // ensure that 0 is never used
	for {
		select {
		case conn.serial <- s:
			used[s] = true
			s++
			for used[s] {
				s++
			}
		case n := <-conn.serialUsed:
			delete(used, n)
		}
	}
}

// Object returns the object identified by the given destination name and path.
func (conn *Connection) Object(dest string, path ObjectPath) *Object {
	if !path.IsValid() {
		panic("invalid DBus path")
	}
	return &Object{conn, dest, path}
}

// Send the given message to the message bus. You usually don't need to use
// this; use the higher-level equivalents (Call, Emit and Export) instead.
// The returned cookie is nil if msg isn't a message call or if NoReplyExpected
// is set.
//
// The serial member is set to a unique serial before sending.
func (conn *Connection) Send(msg *Message) Cookie {
	if err := msg.IsValid(); err != nil {
		c := make(chan *Reply, 1)
		c <- &Reply{nil, err}
		return Cookie(c)
	}
	msg.Serial = <-conn.serial
	if msg.Type == TypeMethodCall && msg.Flags&FlagNoReplyExpected == 0 {
		conn.repliesLck.Lock()
		c := make(chan *Reply, 1)
		conn.replies[msg.Serial] = c
		conn.repliesLck.Unlock()
		conn.out <- msg
		return Cookie(c)
	}
	conn.out <- msg
	return nil
}

// Signal sets the channel to which all received signal messages are forwarded.
// The caller has to make sure that c is sufficiently buffered; if a message
// arrives when a write to c is not possible, it is discarded.
//
// The channel can be reset by passing nil.
//
// This channel is "overwritten" by Eavesdrop; i.e., if there currently is a
// channel for eavesdropped messages, this channel receives all signals, and the
// channel passed to Signal will not receive any signals.
//
// If the connection is closed by the server or a call to Close, the channel is
// also closed.
func (conn *Connection) Signal(c chan Signal) {
	conn.signalsLck.Lock()
	conn.signals = c
	conn.signalsLck.Unlock()
}

// Error represents a DBus message of type Error.
type Error struct {
	Name string
	Body []interface{}
}

func (e Error) Error() string {
	if len(e.Body) > 1 {
		s, ok := e.Body[0].(string)
		if ok {
			return s
		}
	}
	return e.Name
}

// Signal represents a DBus message of type Signal.
type Signal struct {
	Name string
	Body []interface{}
}

// getKey gets a key from a server address. Returns "" on error / not found...
func getKey(s, key string) string {
	i := strings.IndexRune(s, ':')
	if i == -1 {
		return ""
	}
	s = s[i+1:]
	i = strings.Index(s, key)
	if i == -1 {
		return ""
	}
	if i+len(key)+1 >= len(s) || s[i+len(key)] != '=' {
		return ""
	}
	j := strings.Index(s, ",")
	if j == -1 {
		j = len(s)
	}
	return s[i+len(key)+1 : j]
}

// dereferenceAll returns a slice that, assuming that vs is a slice of pointers
// of arbitrary types, containes the values that are obtained from dereferencing
// all elements in vs.
func dereferenceAll(vs []interface{}) []interface{} {
	for i := range vs {
		v := reflect.ValueOf(vs[i])
		v = v.Elem()
		vs[i] = v.Interface()
	}
	return vs
}