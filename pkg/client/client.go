package socketio_client

import (
		"log"
		"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Query        map[string]string //url的附加的参数
	Header       http.Header
	Addr         string
	Path         string
	Transport    string //protocol name string,websocket polling...
	State        State
	PingTimeout  time.Duration
	PingInterval time.Duration
}

type Schema string

const (
	Http         Schema = "http"
	Https        Schema = "https"
	Ws           Schema = "ws"
	Wss          Schema = "wss"
	SocketIoPath        = "/socket.io"
	defaultAddr         = "localhost:8000"
)

func (s Schema) String() string {
	return string(s)
}

type Client struct {
	opts *Options

	conn *clientConn

	eventsLock sync.RWMutex
	events     map[string]*caller
	ackMap     map[int]*caller
	id         int
	namespace  string
}

type Option func(*Options)

func WithAddr(addr string) Option {
	return func(options *Options) {
		options.Addr = addr
	}
}

func WithTransport(transport string) Option {
	return func(options *Options) {
		options.Transport = transport
	}
}

func WithQuery(query map[string]string) Option {
	return func(options *Options) {
		options.Query = query
	}
}

func WithQueryKvs(kvs ...string) Option {
		return func(options *Options) {
				var (
					argc =len(kvs)
					kv = make(map[string]string)
				)
				for i:=0;i<argc; {
						if i+1>=argc {
								break
						}
						kv[kvs[i]] = kvs[i+1]
						i=i+2
				}
				options.Query = kv
		}
}

func WithHeader(header http.Header) Option {
	return func(options *Options) {
		options.Header = header
	}
}

func WithPath(path string) Option {
	return func(options *Options) {
		options.Path = path
	}
}

func NewOptions(opts ...Option) *Options {
	var opt = &Options{
		Path:         SocketIoPath,
		Addr:         defaultAddr,
		Header:       http.Header{},
		Query:        map[string]string{},
		State:        StateNormal,
		Transport: "websocket",
		PingTimeout:  60000 * time.Millisecond,
		PingInterval: 25000 * time.Millisecond,
	}
	for _, o := range opts {
		o(opt)
	}
	return opt
}

func NewClient(opts ...Option) (client *Client, err error) {
	var args = NewOptions(opts...)
	addr, err := url.Parse(args.Addr)
	if err != nil {
		return
	}
	if args.Path != addr.Path && args.Path != "" {
		addr.Path = path.Join(args.Path, addr.Path)
	}
	addr.Path = addr.EscapedPath()
	if !strings.HasSuffix(addr.Path, "/") {
		addr.Path += "/"
	}
	q := addr.Query()
	for k, v := range args.Query {
		q.Set(k, v)
	}
	addr.RawQuery = q.Encode()
	log.Printf("uri=%s\n",addr.String())
	socket, err := newClientConn(args, addr)
	if err != nil {
		return
	}

	client = &Client{
		opts:      args,
		conn:      socket,
		namespace: "/",
		events:    make(map[string]*caller),
		ackMap:    make(map[int]*caller),
	}

	client.start()
	return
}

func (client *Client) Io(ns string) *Client {
	var nsCli = &Client{
		namespace:  ns,
		opts:       client.opts,
		conn:       client.conn,
		eventsLock: sync.RWMutex{},
		events:     make(map[string]*caller),
		ackMap:     make(map[int]*caller),
		id:         client.id,
	}
	nsCli.start()
	return nsCli
}

func (client *Client) start() {
	go client.readLoop()
}

func (client *Client) Namespace() string {
	return client.namespace
}

func (client *Client) On(message string, f interface{}) (err error) {
	c, err := newCaller(f)
	if err != nil {
		return
	}
	client.eventsLock.Lock()
	client.events[message] = c
	client.eventsLock.Unlock()
	return
}

func (client *Client) Emit(message string, args ...interface{}) (err error) {
	var c *caller
	if l := len(args); l > 0 {
		fv := reflect.ValueOf(args[l-1])
		if fv.Kind() == reflect.Func {
			var err error
			c, err = newCaller(args[l-1])
			if err != nil {
				return err
			}
			args = args[:l-1]
		}
	}
	args = append([]interface{}{message}, args...)
	if c != nil {
		id, err := client.sendId(args)
		if err != nil {
			return err
		}
		client.eventsLock.Lock()
		client.ackMap[id] = c
		client.eventsLock.Unlock()
		return nil
	}
	return client.send(args)
}

func (client *Client) sendConnect() error {
	packet := packet{
		Type: _CONNECT,
		Id:   -1,
		NSP:  client.namespace,
	}
	encoder := newEncoder(client.conn)
	return encoder.Encode(packet)
}

func (client *Client) sendId(args []interface{}) (int, error) {
	client.eventsLock.Lock()
	packet := packet{
		Type: _EVENT,
		Id:   client.id,
		NSP:  client.namespace,
		Data: args,
	}
	client.id++
	if client.id < 0 {
		client.id = 0
	}
	client.eventsLock.Unlock()

	encoder := newEncoder(client.conn)
	err := encoder.Encode(packet)
	if err != nil {
		return -1, nil
	}
	return packet.Id, nil
}

func (client *Client) send(args []interface{}) error {
	packet := packet{
		Type: _EVENT,
		Id:   -1,
		NSP:  client.namespace,
		Data: args,
	}
	encoder := newEncoder(client.conn)
	return encoder.Encode(packet)
}

func (client *Client) onPacket(decoder *decoder, packet *packet) ([]interface{}, error) {
	var message string
	switch packet.Type {
	case _CONNECT:
		message = "connection"
	case _DISCONNECT:
		message = "disconnection"
	case _ERROR:
		message = "error"
	case _ACK:
		fallthrough
	case _BINARY_ACK:
		return nil, client.onAck(packet.Id, decoder, packet)
	default:
		message = decoder.Message()
	}
	client.eventsLock.RLock()
	c, ok := client.events[message]
	client.eventsLock.RUnlock()
	if !ok {
		// If the message is not recognized by the server, the decoder.currentCloser
		// needs to be closed otherwise the server will be stuck until the e
		decoder.Close()
		return nil, nil
	}
	args := c.GetArgs()
	olen := len(args)
	if decoder != nil && olen > 0 {
		packet.Data = &args
		if err := decoder.DecodeData(packet); err != nil {
			return nil, err
		}
	}
	for i := len(args); i < olen; i++ {
		args = append(args, nil)
	}

	retV := c.Call(args)
	if len(retV) == 0 {
		return nil, nil
	}

	var err error
	if last, ok := retV[len(retV)-1].Interface().(error); ok {
		err = last
		retV = retV[0 : len(retV)-1]
	}
	ret := make([]interface{}, len(retV))
	for i, v := range retV {
		ret[i] = v.Interface()
	}
	return ret, err
}

func (client *Client) onAck(id int, decoder *decoder, packet *packet) error {
	client.eventsLock.RLock()
	c, ok := client.ackMap[id]
	client.eventsLock.RUnlock()
	if !ok {
		return nil
	}
	delete(client.ackMap, id)

	args := c.GetArgs()
	packet.Data = &args
	if err := decoder.DecodeData(packet); err != nil {
		return err
	}
	c.Call(args)
	return nil
}

func (client *Client) readLoop() error {
	defer func() {
		p := packet{
			Type: _DISCONNECT,
			Id:   -1,
		}
		client.onPacket(nil, &p)
	}()

	for {
		decoder := newDecoder(client.conn)
		var p packet
		if err := decoder.Decode(&p); err != nil {
			return err
		}
		ret, err := client.onPacket(decoder, &p)
		if err != nil {
			return err
		}
		switch p.Type {
		case _CONNECT:
			client.namespace = p.NSP
			// !!!下面这个不能有，否则会有死循环
			//client.sendConnect()
		case _BINARY_EVENT:
			fallthrough
		case _EVENT:
			if p.Id >= 0 {
				p := packet{
					Type: _ACK,
					Id:   p.Id,
					NSP:  client.namespace,
					Data: ret,
				}
				encoder := newEncoder(client.conn)
				if err := encoder.Encode(p); err != nil {
					return err
				}
			}
		case _DISCONNECT:
			return nil
		}
	}
}
