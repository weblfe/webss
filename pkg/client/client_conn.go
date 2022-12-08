package socketio_client

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zhouhui8915/engine.io-go/message"
	"github.com/zhouhui8915/engine.io-go/parser"
	"github.com/zhouhui8915/engine.io-go/polling"
	"github.com/zhouhui8915/engine.io-go/transport"
	"github.com/zhouhui8915/engine.io-go/websocket"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var InvalidError = errors.New("invalid transport")

var transports = []string{"polling", "websocket"}

var creaters map[string]transport.Creater

func init() {
	creaters = make(map[string]transport.Creater)

	for _, t := range transports {
		switch t {
		case "polling":
			creaters[t] = polling.Creater
		case "websocket":
			creaters[t] = websocket.Creater
		}
	}
}

type MessageType message.MessageType

const (
	MessageBinary MessageType = MessageType(message.MessageBinary)
	MessageText   MessageType = MessageType(message.MessageText)
)

type State int

const (
	StateUnknown State = iota
	StateNormal
	StateUpgrading
	StateClosing
	StateClosed
)

type clientConn struct {
	id              string
	options         *Options
	url             *url.URL
	request         *http.Request
	writerLocker    sync.Mutex
	transportLocker sync.RWMutex
	currentName     string
	current         transport.Client
	upgradingName   string
	upgrading       transport.Client
	stateLocker     sync.RWMutex
	readerChan      chan *connReader
	state           State
	pingTimeout     time.Duration
	pingInterval    time.Duration
	pingChan        chan bool
}

func newClientConn(opts *Options, u *url.URL) (client *clientConn, err error) {
	if opts.Transport == "" {
		opts.Transport = "websocket"
	}

	_, exists := creaters[opts.Transport]
	if !exists {
		return nil, InvalidError
	}

	client = &clientConn{
		url:          u,
		options:      opts,
		state:        opts.State,
		pingTimeout:  opts.PingTimeout,
		pingInterval: opts.PingInterval,
		pingChan:     make(chan bool),
		readerChan:   make(chan *connReader),
	}

	err = client.onOpen()
	if err != nil {
		return
	}

	go client.pingLoop()
	go client.readLoop()

	return
}

func (c *clientConn) Id() string {
	return c.id
}

func (c *clientConn) Request() *http.Request {
	return c.request
}

func (c *clientConn) NextReader() (MessageType, io.ReadCloser, error) {
	if c.getState() == StateClosed {
		return MessageBinary, nil, io.EOF
	}
	ret := <-c.readerChan
	if ret == nil {
		return MessageBinary, nil, io.EOF
	}
	return MessageType(ret.MessageType()), ret, nil
}

func (c *clientConn) NextWriter(t MessageType) (io.WriteCloser, error) {
	switch c.getState() {
	case StateUpgrading:
		for i := 0; i < 30; i++ {
			time.Sleep(50 * time.Millisecond)
			if c.getState() != StateUpgrading {
				break
			}
		}
		if c.getState() == StateUpgrading {
			return nil, fmt.Errorf("upgrading")
		}
	case StateNormal:
	default:
		return nil, io.EOF
	}
	c.writerLocker.Lock()
	ret, err := c.getCurrent().NextWriter(message.MessageType(t), parser.MESSAGE)
	if err != nil {
		c.writerLocker.Unlock()
		return ret, err
	}
	writer := newConnWriter(ret, &c.writerLocker)
	return writer, err
}

func (c *clientConn) Close() error {
	if c.getState() != StateNormal && c.getState() != StateUpgrading {
		return nil
	}
	if c.upgrading != nil {
		c.upgrading.Close()
	}
	c.writerLocker.Lock()
	if w, err := c.getCurrent().NextWriter(message.MessageText, parser.CLOSE); err == nil {
		writer := newConnWriter(w, &c.writerLocker)
		writer.Close()
	} else {
		c.writerLocker.Unlock()
	}
	if err := c.getCurrent().Close(); err != nil {
		return err
	}
	c.setState(StateClosing)
	return nil
}

func (c *clientConn) OnPacket(r *parser.PacketDecoder) {
	if s := c.getState(); s != StateNormal && s != StateUpgrading {
		return
	}
	switch r.Type() {
	case parser.OPEN:
	case parser.CLOSE:
		c.getCurrent().Close()
	case parser.PING:
		t := c.getCurrent()
		u := c.getUpgrade()
		newWriter := t.NextWriter
		c.writerLocker.Lock()
		if u != nil {
			if w, _ := t.NextWriter(message.MessageText, parser.NOOP); w != nil {
				w.Close()
			}
			newWriter = u.NextWriter
		}
		if w, _ := newWriter(message.MessageText, parser.PONG); w != nil {
			io.Copy(w, r)
			w.Close()
		}
		c.writerLocker.Unlock()
		fallthrough
	case parser.PONG:
		c.pingChan <- true
		if c.getState() == StateUpgrading {
			p := make([]byte, 64)
			_, err := r.Read(p)
			if err == nil && strings.Contains(string(p), "probe") {
				c.writerLocker.Lock()
				w, _ := c.getUpgrade().NextWriter(message.MessageText, parser.UPGRADE)
				if w != nil {
					io.Copy(w, r)
					w.Close()
				}
				c.writerLocker.Unlock()

				c.upgraded()

			}
		}
	case parser.MESSAGE:
		closeChan := make(chan struct{})
		c.readerChan <- newConnReader(r, closeChan)
		<-closeChan
		close(closeChan)
		r.Close()
	case parser.UPGRADE:
		c.upgraded()
	case parser.NOOP:
	}
}

func (c *clientConn) OnClose(server transport.Client) {
	if t := c.getUpgrade(); server == t {
		c.setUpgrading("", nil)
		t.Close()
		return
	}
	t := c.getCurrent()
	if server != t {
		return
	}
	t.Close()
	if t := c.getUpgrade(); t != nil {
		t.Close()
		c.setUpgrading("", nil)
	}
	c.setState(StateClosed)
	close(c.readerChan)
	close(c.pingChan)
}

func (c *clientConn) onOpen() error {

	var err error
	c.request, err = http.NewRequest("GET", c.url.String(), nil)
	if err != nil {
		return err
	}

	creater, exists := creaters["polling"]
	if !exists {
		return InvalidError
	}

	q := c.request.URL.Query()
	q.Set("transport", "polling")
	c.request.URL.RawQuery = q.Encode()
	if c.options.Header != nil {
		c.request.Header = c.options.Header
	}

	transport, err := creater.Client(c.request)
	if err != nil {
		return err
	}
	c.setCurrent("polling", transport)

	pack, err := c.getCurrent().NextReader()
	if err != nil {
		return err
	}

	p := make([]byte, 4096)
	l, err := pack.Read(p)
	if err != nil {
		return err
	}
	//fmt.Println(string(p))

	type connectionInfo struct {
		Sid          string        `json:"sid"`
		Upgrades     []string      `json:"upgrades"`
		PingInterval time.Duration `json:"pingInterval"`
		PingTimeout  time.Duration `json:"pingTimeout"`
	}

	var msg connectionInfo
	err = json.Unmarshal(p[:l], &msg)
	if err != nil {
		return err
	}
	msg.PingInterval *= 1000 * 1000
	msg.PingTimeout *= 1000 * 1000

	//fmt.Println(msg)

	c.pingInterval = msg.PingInterval
	c.pingTimeout = msg.PingTimeout
	c.id = msg.Sid

	c.getCurrent().Close()

	q.Set("sid", c.id)
	c.request.URL.RawQuery = q.Encode()

	transport, err = creater.Client(c.request)
	if err != nil {
		return err
	}
	c.setCurrent("polling", transport)

	pack, err = c.getCurrent().NextReader()
	if err != nil {
		return err
	}

	p2 := make([]byte, 4096)
	l, err = pack.Read(p2)
	if err != nil {
		return err
	}
	//fmt.Println(string(p2))

	if c.options.Transport == "polling" {
		//over
	} else if c.options.Transport == "websocket" {
		//upgrade
		creater, exists = creaters["websocket"]
		if !exists {
			return InvalidError
		}

		if c.request.URL.Scheme == "https" {
			c.request.URL.Scheme = "wss"
		} else {
			c.request.URL.Scheme = "ws"
		}
		q.Set("sid", c.id)
		q.Set("transport", "websocket")
		c.request.URL.RawQuery = q.Encode()

		transport, err = creater.Client(c.request)
		if err != nil {
			return err
		}
		c.setUpgrading("websocket", transport)

		w, err := c.getUpgrade().NextWriter(message.MessageText, parser.PING)
		if err != nil {
			return err
		}
		w.Write([]byte("probe"))
		w.Close()
	} else {
		return InvalidError
	}

	//fmt.Println("end")

	return nil
}

func (c *clientConn) getCurrent() transport.Client {
	c.transportLocker.RLock()
	defer c.transportLocker.RUnlock()

	return c.current
}

func (c *clientConn) getUpgrade() transport.Client {
	c.transportLocker.RLock()
	defer c.transportLocker.RUnlock()

	return c.upgrading
}

func (c *clientConn) setCurrent(name string, s transport.Client) {
	c.transportLocker.Lock()
	defer c.transportLocker.Unlock()

	c.currentName = name
	c.current = s
}

func (c *clientConn) setUpgrading(name string, s transport.Client) {
	c.transportLocker.Lock()
	defer c.transportLocker.Unlock()

	c.upgradingName = name
	c.upgrading = s
	c.setState(StateUpgrading)
}

func (c *clientConn) upgraded() {
	c.transportLocker.Lock()

	current := c.current
	c.current = c.upgrading
	c.currentName = c.upgradingName
	c.upgrading = nil
	c.upgradingName = ""

	c.transportLocker.Unlock()

	current.Close()
	c.setState(StateNormal)
}

func (c *clientConn) getState() State {
	c.stateLocker.RLock()
	defer c.stateLocker.RUnlock()
	return c.state
}

func (c *clientConn) setState(state State) {
	c.stateLocker.Lock()
	defer c.stateLocker.Unlock()
	c.state = state
}

func (c *clientConn) pingLoop() {
	lastPing := time.Now()
	lastTry := lastPing
	for {
		now := time.Now()
		pingDiff := now.Sub(lastPing)
		tryDiff := now.Sub(lastTry)
		select {
		case ok := <-c.pingChan:
			if !ok {
				return
			}
			lastPing = time.Now()
			lastTry = lastPing
		case <-time.After(c.pingInterval - tryDiff):
			c.writerLocker.Lock()
			if w, _ := c.getCurrent().NextWriter(message.MessageText, parser.PING); w != nil {
				writer := newConnWriter(w, &c.writerLocker)
				writer.Close()
			} else {
				c.writerLocker.Unlock()
			}
			lastTry = time.Now()
		case <-time.After(c.pingTimeout - pingDiff):
			c.Close()
			return
		}
	}
}

func (c *clientConn) readLoop() {

	current := c.getCurrent()

	defer func() {
		c.OnClose(current)
	}()

	for {
		current = c.getCurrent()
		if c.getUpgrade() != nil {
			current = c.getUpgrade()
		}

		pack, err := current.NextReader()
		if err != nil {
			return
		}
		c.OnPacket(pack)
		pack.Close()
	}
}
