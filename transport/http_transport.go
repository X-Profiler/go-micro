package transport

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	maddr "go-micro.dev/v4/util/addr"
	"go-micro.dev/v4/util/buf"
	mnet "go-micro.dev/v4/util/net"
	mls "go-micro.dev/v4/util/tls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type httpTransport struct {
	opts Options
}

type httpTransportClient struct {
	ht       *httpTransport
	addr     string
	conn     net.Conn
	dialOpts DialOptions
	once     sync.Once

	sync.RWMutex

	// request must be stored for response processing
	r      chan *http.Request
	bl     []*http.Request
	buff   *bufio.Reader
	closed bool

	// local/remote ip
	local  string
	remote string
}

type httpTransportSocket struct {
	ht *httpTransport
	w  http.ResponseWriter
	r  *http.Request
	rw *bufio.ReadWriter

	mtx sync.RWMutex

	// the hijacked when using http 1
	conn net.Conn
	// for the first request
	ch chan *http.Request

	// h2 things
	buf *bufio.Reader
	// indicate if socket is closed
	closed chan bool

	// local/remote ip
	local  string
	remote string
}

type httpTransportListener struct {
	ht       *httpTransport
	listener net.Listener
}

func (h *httpTransportClient) Local() string {
	return h.local
}

func (h *httpTransportClient) Remote() string {
	return h.remote
}

func (h *httpTransportClient) Send(m *Message) error {
	header := make(http.Header)

	for k, v := range m.Header {
		header.Set(k, v)
	}

	b := buf.New(bytes.NewBuffer(m.Body))
	defer b.Close()

	req := &http.Request{
		Method: http.MethodPost,
		URL: &url.URL{
			Scheme: "http",
			Host:   h.addr,
		},
		Header:        header,
		Body:          b,
		ContentLength: int64(b.Len()),
		Host:          h.addr,
	}

	if !h.dialOpts.Stream {
		h.Lock()
		if h.closed {
			h.Unlock()
			return io.EOF
		}

		h.bl = append(h.bl, req)

		select {
		case h.r <- h.bl[0]:
			h.bl = h.bl[1:]
		default:
		}
		h.Unlock()
	}

	// set timeout if its greater than 0
	if h.ht.opts.Timeout > time.Duration(0) {
		if err := h.conn.SetDeadline(time.Now().Add(h.ht.opts.Timeout)); err != nil {
			return err
		}
	}

	return req.Write(h.conn)
}

// Recv receives a message.
func (h *httpTransportClient) Recv(msg *Message) error {
	if msg == nil {
		return errors.New("message passed in is nil")
	}

	var req *http.Request

	if !h.dialOpts.Stream {
		rc, ok := <-h.r
		if !ok {
			h.Lock()
			if len(h.bl) == 0 {
				h.Unlock()
				return io.EOF
			}

			rc = h.bl[0]
			h.bl = h.bl[1:]
			h.Unlock()
		}

		req = rc
	}

	// set timeout if its greater than 0
	if h.ht.opts.Timeout > time.Duration(0) {
		if err := h.conn.SetDeadline(time.Now().Add(h.ht.opts.Timeout)); err != nil {
			return err
		}
	}

	h.Lock()
	defer h.Unlock()

	if h.closed {
		return io.EOF
	}

	rsp, err := http.ReadResponse(h.buff, req)
	if err != nil {
		return err
	}

	defer rsp.Body.Close()

	b, err := io.ReadAll(rsp.Body)
	if err != nil {
		return err
	}

	if rsp.StatusCode != http.StatusOK {
		return errors.New(rsp.Status + ": " + string(b))
	}

	msg.Body = b

	if msg.Header == nil {
		msg.Header = make(map[string]string, len(rsp.Header))
	}

	for k, v := range rsp.Header {
		if len(v) > 0 {
			msg.Header[k] = v[0]
		} else {
			msg.Header[k] = ""
		}
	}

	return nil
}

func (h *httpTransportClient) Close() error {
	if !h.dialOpts.Stream {
		h.once.Do(func() {
			h.Lock()
			h.buff.Reset(nil)
			h.closed = true
			h.Unlock()
			close(h.r)
		})

		return h.conn.Close()
	}

	err := h.conn.Close()
	h.once.Do(func() {
		h.Lock()
		h.buff.Reset(nil)
		h.closed = true
		h.Unlock()
		close(h.r)
	})

	return err
}

func (h *httpTransportSocket) Local() string {
	return h.local
}

func (h *httpTransportSocket) Remote() string {
	return h.remote
}

func (h *httpTransportSocket) Recv(msg *Message) error {
	if msg == nil {
		return errors.New("message passed in is nil")
	}

	if msg.Header == nil {
		msg.Header = make(map[string]string, len(h.r.Header))
	}

	// process http 1
	if h.r.ProtoMajor == 1 {
		// set timeout if its greater than 0
		if h.ht.opts.Timeout > time.Duration(0) {
			h.conn.SetDeadline(time.Now().Add(h.ht.opts.Timeout))
		}

		var r *http.Request

		select {
		// get first request
		case r = <-h.ch:
		// read next request
		default:
			rr, err := http.ReadRequest(h.rw.Reader)
			if err != nil {
				return err
			}

			r = rr
		}

		// read body
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}

		// set body
		r.Body.Close()

		msg.Body = b

		// set headers
		for k, v := range r.Header {
			if len(v) > 0 {
				msg.Header[k] = v[0]
			} else {
				msg.Header[k] = ""
			}
		}

		// return early early
		return nil
	}

	// only process if the socket is open
	select {
	case <-h.closed:
		return io.EOF
	default:
		// no op
	}

	// processing http2 request
	// read streaming body

	// set max buffer size
	// TODO: adjustable buffer size
	buf := make([]byte, 4*1024*1024)

	// read the request body
	n, err := h.buf.Read(buf)
	// not an eof error
	if err != nil {
		return err
	}

	// check if we have data
	if n > 0 {
		msg.Body = buf[:n]
	}

	// set headers
	for k, v := range h.r.Header {
		if len(v) > 0 {
			msg.Header[k] = v[0]
		} else {
			msg.Header[k] = ""
		}
	}

	// set path
	msg.Header[":path"] = h.r.URL.Path

	return nil
}

func (h *httpTransportSocket) Send(msg *Message) error {
	if h.r.ProtoMajor == 1 {
		// make copy of header
		hdr := make(http.Header)
		for k, v := range h.r.Header {
			hdr[k] = v
		}

		rsp := &http.Response{
			Header:        hdr,
			Body:          io.NopCloser(bytes.NewReader(msg.Body)),
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			ContentLength: int64(len(msg.Body)),
		}

		for k, v := range msg.Header {
			rsp.Header.Set(k, v)
		}

		// set timeout if its greater than 0
		if h.ht.opts.Timeout > time.Duration(0) {
			h.conn.SetDeadline(time.Now().Add(h.ht.opts.Timeout))
		}

		return rsp.Write(h.conn)
	}

	// only process if the socket is open
	select {
	case <-h.closed:
		return io.EOF
	default:
		// no op
	}

	// we need to lock to protect the write
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	// set headers
	for k, v := range msg.Header {
		h.w.Header().Set(k, v)
	}

	// write request
	_, err := h.w.Write(msg.Body)

	// flush the trailers
	h.w.(http.Flusher).Flush()

	return err
}

func (h *httpTransportSocket) error(m *Message) error {
	if h.r.ProtoMajor == 1 {
		rsp := &http.Response{
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(m.Body)),
			Status:        "500 Internal Server Error",
			StatusCode:    http.StatusInternalServerError,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			ContentLength: int64(len(m.Body)),
		}

		for k, v := range m.Header {
			rsp.Header.Set(k, v)
		}

		return rsp.Write(h.conn)
	}

	return nil
}

func (h *httpTransportSocket) Close() error {
	h.mtx.Lock()
	defer h.mtx.Unlock()
	select {
	case <-h.closed:
		return nil
	default:
		// close the channel
		close(h.closed)

		// close the buffer
		h.r.Body.Close()

		// close the connection
		if h.r.ProtoMajor == 1 {
			return h.conn.Close()
		}
	}

	return nil
}

func (h *httpTransportListener) Addr() string {
	return h.listener.Addr().String()
}

func (h *httpTransportListener) Close() error {
	return h.listener.Close()
}

func (h *httpTransportListener) Accept(fn func(Socket)) error {
	// create handler mux
	mux := http.NewServeMux()

	// register our transport handler
	mux.HandleFunc("/", func(rsp http.ResponseWriter, req *http.Request) {
		var buf *bufio.ReadWriter
		var con net.Conn

		// read a regular request
		if req.ProtoMajor == 1 {
			b, err := io.ReadAll(req.Body)
			if err != nil {
				http.Error(rsp, err.Error(), http.StatusInternalServerError)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(b))
			// hijack the conn
			hj, ok := rsp.(http.Hijacker)
			if !ok {
				// we're screwed
				http.Error(rsp, "cannot serve conn", http.StatusInternalServerError)
				return
			}

			conn, bufrw, err := hj.Hijack()
			if err != nil {
				http.Error(rsp, err.Error(), http.StatusInternalServerError)
				return
			}
			defer conn.Close()
			buf = bufrw
			con = conn
		}

		// buffered reader
		bufr := bufio.NewReader(req.Body)

		// save the request
		ch := make(chan *http.Request, 1)
		ch <- req

		// create a new transport socket
		sock := &httpTransportSocket{
			ht:     h.ht,
			w:      rsp,
			r:      req,
			rw:     buf,
			buf:    bufr,
			ch:     ch,
			conn:   con,
			local:  h.Addr(),
			remote: req.RemoteAddr,
			closed: make(chan bool),
		}

		// execute the socket
		fn(sock)
	})

	// get optional handlers
	if h.ht.opts.Context != nil {
		handlers, ok := h.ht.opts.Context.Value("http_handlers").(map[string]http.Handler)
		if ok {
			for pattern, handler := range handlers {
				mux.Handle(pattern, handler)
			}
		}
	}

	// default http2 server
	srv := &http.Server{
		Handler: mux,
	}

	// insecure connection use h2c
	if !(h.ht.opts.Secure || h.ht.opts.TLSConfig != nil) {
		srv.Handler = h2c.NewHandler(mux, &http2.Server{})
	}

	// begin serving
	return srv.Serve(h.listener)
}

func (h *httpTransport) Dial(addr string, opts ...DialOption) (Client, error) {
	dopts := DialOptions{
		Timeout: DefaultDialTimeout,
	}

	for _, opt := range opts {
		opt(&dopts)
	}

	var (
		conn net.Conn
		err  error
	)

	// TODO: support dial option here rather than using internal config
	if h.opts.Secure || h.opts.TLSConfig != nil {
		config := h.opts.TLSConfig
		if config == nil {
			config = &tls.Config{
				InsecureSkipVerify: true,
			}
		}

		config.NextProtos = []string{"http/1.1"}

		conn, err = newConn(func(addr string) (net.Conn, error) {
			return tls.DialWithDialer(&net.Dialer{Timeout: dopts.Timeout}, "tcp", addr, config)
		})(addr)
	} else {
		conn, err = newConn(func(addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, dopts.Timeout)
		})(addr)
	}

	if err != nil {
		return nil, err
	}

	return &httpTransportClient{
		ht:       h,
		addr:     addr,
		conn:     conn,
		buff:     bufio.NewReader(conn),
		dialOpts: dopts,
		r:        make(chan *http.Request, 100),
		local:    conn.LocalAddr().String(),
		remote:   conn.RemoteAddr().String(),
	}, nil
}

func (h *httpTransport) Listen(addr string, opts ...ListenOption) (Listener, error) {
	var options ListenOptions
	for _, o := range opts {
		o(&options)
	}

	var (
		list net.Listener
		err  error
	)

	if listener := getNetListener(&options); listener != nil {
		fn := func(addr string) (net.Listener, error) {
			return listener, nil
		}

		list, err = mnet.Listen(addr, fn)
	} else if h.opts.Secure || h.opts.TLSConfig != nil {
		config := h.opts.TLSConfig

		fn := func(addr string) (net.Listener, error) {
			if config == nil {
				hosts := []string{addr}

				// check if its a valid host:port
				if host, _, err := net.SplitHostPort(addr); err == nil {
					if len(host) == 0 {
						hosts = maddr.IPs()
					} else {
						hosts = []string{host}
					}
				}

				// generate a certificate
				cert, err := mls.Certificate(hosts...)
				if err != nil {
					return nil, err
				}
				config = &tls.Config{Certificates: []tls.Certificate{cert}}
			}
			return tls.Listen("tcp", addr, config)
		}

		list, err = mnet.Listen(addr, fn)
	} else {
		fn := func(addr string) (net.Listener, error) {
			return net.Listen("tcp", addr)
		}

		list, err = mnet.Listen(addr, fn)
	}

	if err != nil {
		return nil, err
	}

	return &httpTransportListener{
		ht:       h,
		listener: list,
	}, nil
}

func (h *httpTransport) Init(opts ...Option) error {
	for _, o := range opts {
		o(&h.opts)
	}

	return nil
}

func (h *httpTransport) Options() Options {
	return h.opts
}

func (h *httpTransport) String() string {
	return "http"
}

func NewHTTPTransport(opts ...Option) *httpTransport {
	var options Options
	for _, o := range opts {
		o(&options)
	}

	return &httpTransport{opts: options}
}
