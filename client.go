package http2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gospider007/http1"
	"github.com/gospider007/tools"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2/hpack"
)

type Http2ClientConn struct {
	tconn             net.Conn
	loop              *http2clientConnReadLoop
	http2clientStream *http2clientStream

	wmu sync.Mutex

	bw *bufio.Writer
	fr *http2Framer

	henc *hpack.Encoder
	hbuf bytes.Buffer

	spec   gospiderOption
	inflow http2inflow
	flow   http2outflow

	streamID uint32

	flowNotices chan struct{}
	ctx         context.Context
	cnl         context.CancelCauseFunc
	respDone    chan *respC
	shutdownErr error
	bodyContext context.Context
}

func (obj *Http2ClientConn) SetBodyContext(ctx context.Context) {
	obj.bodyContext = ctx
}
func (obj *Http2ClientConn) BodyContext() context.Context {
	return obj.bodyContext
}

func (obj *Http2ClientConn) Context() context.Context {
	return obj.ctx
}
func (obj *Http2ClientConn) Stream() io.ReadWriteCloser {
	return nil
}

type respC struct {
	resp *http.Response
	err  error
}

type http2clientStream struct {
	cc     *Http2ClientConn
	body   *http2clientBody
	ID     uint32
	inflow http2inflow
	flow   http2outflow
}

func (cc *Http2ClientConn) notice() {
	select {
	case cc.flowNotices <- struct{}{}:
	default:
	}
}

func (cc *Http2ClientConn) run() (err error) {
	defer func() {
		cc.CloseWithError(err)
	}()
	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			return tools.WrapError(err, "ReadFrame")
		}
		switch f := f.(type) {
		case *Http2MetaHeadersFrame:
			if cc.http2clientStream == nil {
				return tools.WrapError(errors.New("unexpected meta headers frame"), "headers")
			}
			if cc.http2clientStream.ID != f.StreamID {
				return tools.WrapError(errors.New("not found stream id"), "headers")
			}
			resp, err := cc.loop.handleResponse(cc.http2clientStream, f)
			select {
			case cc.respDone <- &respC{resp: resp, err: err}:
			case <-cc.ctx.Done():
				err = cc.ctx.Err()
			}
			if err != nil {
				return tools.WrapError(err, "handleResponse")
			}
			if cc.shutdownErr != nil && f.StreamEnded() {
				return cc.shutdownErr
			}
		case *Http2DataFrame:
			if err = cc.loop.processData(cc.http2clientStream, f); err != nil {
				return tools.WrapError(err, "processData")
			}
			if cc.shutdownErr != nil && f.StreamEnded() {
				return cc.shutdownErr
			}
		case *Http2GoAwayFrame:
			if f.ErrCode == 0 {
				cc.shutdownErr = tools.WrapError(tools.ErrNoErr, "http2: server sent GOAWAY with close connection ok")
			} else {
				err = fmt.Errorf("http2: server sent GOAWAY with error code %v", f.ErrCode)
				return tools.WrapError(err, "GOAWAY")
			}
		case *Http2RSTStreamFrame:
			err = fmt.Errorf("http2: server sent processResetStream with error code %v", f.ErrCode)
			return tools.WrapError(err, "processResetStream")
		case *Http2SettingsFrame:
			if err = cc.loop.processSettings(f); err != nil {
				return tools.WrapError(err, "processSettings")
			}
		case *Http2PushPromiseFrame:
		case *Http2WindowUpdateFrame:
			if err = cc.loop.processWindowUpdate(f); err != nil {
				return tools.WrapError(err, "processWindowUpdate")
			}
		case *Http2PingFrame:
			if err = cc.loop.processPing(f); err != nil {
				return tools.WrapError(err, "processPing")
			}
		default:
			err = fmt.Errorf("unknown frame type: %T", f)
			return tools.WrapError(err, "run")
		}
	}
}

type gospiderOption struct {
	initialSetting    []Http2Setting
	priority          Http2PriorityParam
	connFlow          uint32
	initialWindowSize uint32
	headerTableSize   uint32
	maxHeaderListSize uint32
	maxFrameSize      uint32
}

func spec2option(h2Spec *Spec) (option gospiderOption) {
	if h2Spec == nil {
		//golang setting: start
		option.initialWindowSize = 4194304
		option.maxFrameSize = 16384
		option.maxHeaderListSize = 10485760
		option.initialSetting = []Http2Setting{
			{ID: 2, Val: 0},
			{ID: 4, Val: option.initialWindowSize},
			{ID: 5, Val: option.maxFrameSize},
			{ID: 6, Val: option.maxHeaderListSize},
		}
		option.priority = Http2PriorityParam{}
		option.connFlow = 1073741824
		option.headerTableSize = 4096
		h2Spec = nil
		//golang setting: end
	} else {
		option.initialSetting = h2Spec.Settings
		option.priority = h2Spec.Priority
		option.headerTableSize = 65536
		option.maxHeaderListSize = 262144
		option.initialWindowSize = 6291456
		option.maxFrameSize = 16384
		if h2Spec.ConnFlow > 0 {
			option.connFlow = h2Spec.ConnFlow
		} else {
			option.connFlow = 15663105
		}
	}
	for _, setting := range option.initialSetting {
		switch setting.ID {
		case Http2SettingHeaderTableSize:
			option.headerTableSize = setting.Val
		case Http2SettingMaxHeaderListSize:
			option.maxHeaderListSize = setting.Val
		case Http2SettingInitialWindowSize:
			option.initialWindowSize = setting.Val
		case Http2SettingMaxFrameSize:
			option.maxFrameSize = setting.Val
		}
	}
	return option
}

func NewConn(conCtx context.Context, reqCtx context.Context, c net.Conn, h2Spec *Spec) (http1.Conn, error) {
	var streamID uint32
	if h2Spec != nil {
		streamID = h2Spec.StreamID
	} else {
		streamID = 1
	}
	spec := spec2option(h2Spec)
	cc := &Http2ClientConn{
		spec:        spec,
		tconn:       c,
		flowNotices: make(chan struct{}, 1),
		streamID:    streamID,
		respDone:    make(chan *respC),
	}
	cc.ctx, cc.cnl = context.WithCancelCause(conCtx)
	cc.bw = bufio.NewWriter(c)
	cc.fr = http2NewFramer(cc.bw, bufio.NewReader(c))
	cc.fr.ReadMetaHeaders = hpack.NewDecoder(cc.spec.headerTableSize, nil)
	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.henc.SetMaxDynamicTableSizeLimit(cc.spec.headerTableSize)
	cc.spec.initialWindowSize = 65535
	cc.flow.add(int32(cc.spec.initialWindowSize))
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		if h2Spec != nil && len(h2Spec.initData) > 0 {
			rawData := []byte(fmt.Sprintf("%s\r\n\r\n%s\r\n\r\n", h2Spec.Pri, h2Spec.Sm))
			rawData = append(rawData, h2Spec.initData...)
			if _, err = cc.bw.Write(rawData); err != nil {
				return
			}
		} else {
			if _, err = cc.bw.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")); err != nil {
				return
			}
			if err = cc.fr.WriteSettings(cc.spec.initialSetting...); err != nil {
				return
			}
			if err = cc.fr.WriteWindowUpdate(0, cc.spec.connFlow); err != nil {
				return
			}
		}
		cc.inflow.init(int32(cc.spec.connFlow) + int32(cc.spec.initialWindowSize))
		if err = cc.bw.Flush(); err != nil {
			return
		}
	}()
	select {
	case <-done:
		if err != nil {
			return cc, err
		}
	case <-reqCtx.Done():
		c.Close()
		return cc, context.Cause(reqCtx)
	}
	cc.loop = &http2clientConnReadLoop{cc: cc}
	go cc.run()
	return cc, nil
}

func (cc *Http2ClientConn) CloseWithError(err error) error {
	cc.cnl(err)
	cc.tconn.Close()
	if cc.http2clientStream != nil {
		cc.http2clientStream.body.CloseWithError(err)
	}
	return nil
}

func http2actualContentLength(req *http.Request) int64 {
	if req.Body == nil || req.Body == http.NoBody {
		return 0
	}
	if req.ContentLength != 0 {
		return req.ContentLength
	}
	return -1
}

type http2clientBody struct {
	c *Http2ClientConn
	r *io.PipeReader
	w *io.PipeWriter
}

func (obj *http2clientBody) Close() error {
	return obj.CloseWithError(nil)
}

func (obj *http2clientBody) CloseWithError(err error) error {
	return obj.w.CloseWithError(err)
}
func (obj *http2clientBody) Read(p []byte) (n int, err error) {
	return obj.r.Read(p)
}
func (obj *http2clientBody) Write(p []byte) (n int, err error) {
	return obj.w.Write(p)
}

func (cc *Http2ClientConn) initStream() error {
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	reader, writer := io.Pipe()
	cs := &http2clientStream{
		cc:   cc,
		body: &http2clientBody{c: cc, r: reader, w: writer},
	}
	cc.http2clientStream = cs
	cs.inflow.init(int32(cc.spec.initialWindowSize))
	cs.flow.add(int32(cc.spec.initialWindowSize))
	cs.flow.setConnFlow(&cc.flow)
	cs.ID = cc.streamID
	cc.streamID += 2
	return nil
}

func (cc *Http2ClientConn) DoRequest(ctx context.Context, req *http.Request, option *http1.Option) (response *http.Response, err error) {
	defer func() {
		if err != nil {
			cc.CloseWithError(err)
		}
	}()
	if err = cc.initStream(); err != nil {
		return nil, err
	}
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeErr := cc.http2clientStream.writeRequest(req, option.OrderHeaders)
		if writeErr != nil {
			cc.CloseWithError(writeErr)
		}
	}()
	for {
		select {
		case respData := <-cc.respDone:
			if respData.err == nil {
				respData.resp.Body.(*http1.Body).SetWriteDone(writeDone)
				if respData.resp.StatusCode == 103 {
					continue
				}
			}
			return respData.resp, respData.err
		case <-ctx.Done():
			cc.CloseWithError(context.Cause(ctx))
			return nil, ctx.Err()
		case <-cc.ctx.Done():
			cc.CloseWithError(context.Cause(cc.ctx))
			return nil, cc.ctx.Err()
		}
	}
}

func (cs *http2clientStream) writeRequest(req *http.Request, orderHeaders []interface {
	Key() string
	Val() any
}) (err error) {
	endStream := http2actualContentLength(req) == 0
	if err = cs.encodeAndWriteHeaders(req, endStream, orderHeaders); err != nil {
		return err
	}
	if !endStream {
		return cs.writeRequestBody(req)
	}
	return
}

func (cs *http2clientStream) encodeAndWriteHeaders(req *http.Request, endStream bool, orderHeaders []interface {
	Key() string
	Val() any
}) error {
	cs.cc.wmu.Lock()
	defer cs.cc.wmu.Unlock()
	hdrs, err := cs.cc.encodeHeaders(req, orderHeaders)
	if err != nil {
		return err
	}
	return cs.cc.writeHeaders(cs.ID, endStream, int(cs.cc.spec.maxFrameSize), hdrs)
}
func (cc *Http2ClientConn) writeHeaders(streamID uint32, endStream bool, maxFrameSize int, hdrs []byte) error {
	first := true
	for len(hdrs) > 0 {
		chunk := hdrs
		if len(chunk) > maxFrameSize {
			chunk = chunk[:maxFrameSize]
		}
		hdrs = hdrs[len(chunk):]
		endHeaders := len(hdrs) == 0
		if first {
			http2HeadersFrameParam := Http2HeadersFrameParam{
				StreamID:      streamID,
				BlockFragment: chunk,
				EndStream:     endStream,
				EndHeaders:    endHeaders,
			}
			if cc.spec.priority.StreamDep != 0 || cc.spec.priority.Weight != 0 || cc.spec.priority.Exclusive {
				http2HeadersFrameParam.Priority = Http2PriorityParam{
					StreamDep: cc.spec.priority.StreamDep,
					Exclusive: cc.spec.priority.Exclusive,
					Weight:    cc.spec.priority.Weight,
				}
			}
			if err := cc.fr.WriteHeaders(http2HeadersFrameParam); err != nil {
				return err
			}
			first = false
		} else {
			if err := cc.fr.WriteContinuation(streamID, endHeaders, chunk); err != nil {
				return err
			}
		}
	}
	return cc.bw.Flush()
}

func (cs *http2clientStream) frameScratchBufferLen(req *http.Request, maxFrameSize int) int {
	const max = 512 << 10
	n := int64(maxFrameSize)
	if n > max {
		n = max
	}
	if cl := http2actualContentLength(req); cl != -1 && cl+1 < n {
		n = cl + 1
	}
	if n < 1 {
		return 1
	}
	return int(n)
}

func (cs *http2clientStream) available(maxBytes int) (taken int32) {
	cs.cc.wmu.Lock()
	defer cs.cc.wmu.Unlock()
	if a := cs.flow.available(); a > 0 {
		take := a
		if int(take) > maxBytes {
			take = int32(maxBytes) // can't truncate int; take is int32
		}
		if take > int32(cs.cc.spec.maxFrameSize) {
			take = int32(cs.cc.spec.maxFrameSize)
		}
		cs.flow.take(take)
		return take
	}
	return 0
}
func (cs *http2clientStream) awaitFlowControl(maxBytes int) (taken int32, err error) {
	for {
		if taken = cs.available(maxBytes); taken > 0 {
			return
		}
		select {
		case <-cs.cc.ctx.Done():
			return 0, context.Cause(cs.cc.ctx)
		case <-cs.cc.flowNotices:
		case <-time.After(time.Second * 30):
			return 0, errors.New("timeout waiting for flow control")
		}
	}
}

func (cs *http2clientStream) writeRequestBody(req *http.Request) (bodyErr error) {
	buf := make([]byte, cs.frameScratchBufferLen(req, int(cs.cc.spec.maxFrameSize)))
	for {
		n, err := req.Body.Read(buf)
		if n > 0 {
			if bodyErr = cs.WriteData(err != nil, buf[:n]); bodyErr != nil {
				return
			}
		} else if err != nil {
			if bodyErr = cs.WriteEndNoData(); bodyErr != nil {
				return
			}
		}
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			bodyErr = err
			return
		}
	}
}

func (cs *http2clientStream) WriteEndNoData() (err error) {
	cs.cc.wmu.Lock()
	defer cs.cc.wmu.Unlock()
	err = cs.cc.fr.WriteData(cs.ID, true, nil)
	if err == nil {
		err = cs.cc.bw.Flush()
	}
	return err
}

func (cs *http2clientStream) WriteData(endStream bool, remain []byte) (err error) {
	if endStream && len(remain) == 0 {
		return cs.WriteEndNoData()
	}
	for len(remain) > 0 && err == nil {
		var allowed int32
		allowed, err = cs.awaitFlowControl(len(remain))
		if err != nil {
			return err
		}
		data := remain[:allowed]
		remain = remain[allowed:]
		sentEnd := endStream && len(remain) == 0
		cs.cc.wmu.Lock()
		err = cs.cc.fr.WriteData(cs.ID, sentEnd, data)
		if err == nil {
			err = cs.cc.bw.Flush()
		}
		cs.cc.wmu.Unlock()
	}
	return
}

func (cc *Http2ClientConn) encodeHeaders(req *http.Request, orderHeaders []interface {
	Key() string
	Val() any
}) ([]byte, error) {
	cc.hbuf.Reset()
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host, err := httpguts.PunycodeHostPort(host)
	if err != nil {
		return nil, err
	}
	var path string
	if req.Method != http.MethodConnect {
		path = req.URL.RequestURI()
		if !http2validPseudoPath(path) {
			path = strings.TrimPrefix(path, req.URL.Scheme+"://"+host)
		}
	}
	enumerateHeaders := func(replaceF func(name, value string)) {
		gospiderHeaders := [][2]string{}
		f := func(name, value string) {
			gospiderHeaders = append(gospiderHeaders, [2]string{
				strings.ToLower(name), value,
			})
		}
		f(":method", req.Method)
		f(":authority", host)
		if req.Method != http.MethodConnect {
			f(":scheme", req.URL.Scheme)
			f(":path", path)
		}
		for k, vv := range req.Header {
			switch strings.ToLower(k) {
			case "host", "content-length", "connection", "proxy-connection", "transfer-encoding", "upgrade", "keep-alive":
			case "cookie":
				for _, v := range vv {
					for _, c := range strings.Split(v, "; ") {
						f("cookie", c)
					}
				}
			default:
				for _, v := range vv {
					f(k, v)
				}
			}
		}
		if contentLength, _ := tools.GetContentLength(req); contentLength >= 0 {
			f("content-length", strconv.FormatInt(contentLength, 10))
		}
		for _, kv := range tools.NewHeadersWithH2(orderHeaders, gospiderHeaders) {
			replaceF(kv[0], kv[1])
		}
	}
	enumerateHeaders(func(name, value string) {
		name = strings.ToLower(name)
		cc.writeHeader(name, value)
	})
	return cc.hbuf.Bytes(), nil
}

func (cc *Http2ClientConn) writeHeader(name, value string) {
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

type http2clientConnReadLoop struct {
	cc *Http2ClientConn
}

func (rl *http2clientConnReadLoop) handleResponse(cs *http2clientStream, f *Http2MetaHeadersFrame) (*http.Response, error) {
	status := f.PseudoValue("status")
	statusCode, err := strconv.Atoi(status)
	if err != nil {
		return nil, errors.New("malformed response from server: malformed non-numeric status pseudo header")
	}
	regularFields := f.RegularFields()
	res := &http.Response{
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		Header:     make(http.Header),
		StatusCode: statusCode,
		Status:     status + " " + http.StatusText(statusCode),
	}
	for _, hf := range regularFields {
		key := http.CanonicalHeaderKey(hf.Name)
		if key == "Trailer" {
			if res.Trailer == nil {
				res.Trailer = make(http.Header)
			}
			for _, f := range strings.Split(hf.Value, ",") {
				if f = textproto.TrimString(f); f != "" {
					res.Trailer[http.CanonicalHeaderKey(f)] = nil
				}
			}
		} else {
			res.Header.Add(key, hf.Value)
		}
	}
	res.ContentLength = -1
	if clens := res.Header["Content-Length"]; len(clens) >= 1 {
		if cl, err := strconv.ParseUint(clens[0], 10, 63); err == nil {
			res.ContentLength = int64(cl)
		}
	} else if f.StreamEnded() {
		res.ContentLength = 0
	}
	if f.StreamEnded() {
		res.Body = http1.NewBody(http.NoBody, cs.cc, nil, nil, false, nil)
		return res, nil
	} else {
		ctx, cnl := context.WithCancelCause(cs.cc.ctx)
		res.Body = http1.NewBody(cs.body, cs.cc, ctx, cnl, false, nil)
		return res, nil
	}
}

func (rl *http2clientConnReadLoop) processData(cs *http2clientStream, f *Http2DataFrame) (err error) {
	if f.Length > 0 {
		if len(f.Data()) > 0 {
			if _, err = cs.body.Write(f.Data()); err != nil {
				return err
			}
		}
		cs.cc.wmu.Lock()
		defer cs.cc.wmu.Unlock()
		connAdd := rl.cc.inflow.add(int32(f.Length))
		streamAdd := cs.inflow.add(int32(f.Length))
		if connAdd > 0 || streamAdd > 0 {
			if connAdd > 0 {
				if err = rl.cc.fr.WriteWindowUpdate(0, uint32(connAdd)); err != nil {
					return err
				}
			}
			if streamAdd > 0 {
				if err = rl.cc.fr.WriteWindowUpdate(cs.ID, uint32(connAdd)); err != nil {
					return err
				}
			}
			if err = rl.cc.bw.Flush(); err != nil {
				return err
			}
		}
	}
	if f.StreamEnded() {
		cs.body.CloseWithError(io.EOF)
	}
	return
}

func (rl *http2clientConnReadLoop) processWindowUpdate(f *Http2WindowUpdateFrame) error {
	rl.cc.wmu.Lock()
	defer rl.cc.wmu.Unlock()
	if f.StreamID == 0 {
		rl.cc.flow.add(int32(f.Increment))
	} else {
		rl.cc.http2clientStream.flow.add(int32(f.Increment))
	}
	rl.cc.notice()
	return nil
}
func (rl *http2clientConnReadLoop) processSettings(f *Http2SettingsFrame) error {
	rl.cc.wmu.Lock()
	defer rl.cc.wmu.Unlock()
	if err := rl.processSettingsNoWrite(f); err != nil {
		return err
	}
	if !f.IsAck() {
		if err := rl.cc.fr.WriteSettingsAck(); err != nil {
			return err
		}
		return rl.cc.bw.Flush()
	}
	return nil
}
func (rl *http2clientConnReadLoop) processSettingsNoWrite(f *Http2SettingsFrame) error {
	return f.ForeachSetting(func(s Http2Setting) error {
		switch s.ID {
		case Http2SettingMaxFrameSize:
			rl.cc.spec.maxFrameSize = s.Val
		case Http2SettingInitialWindowSize:
			if rl.cc.http2clientStream != nil {
				rl.cc.http2clientStream.flow.n = int32(s.Val)
				rl.cc.notice()
			}
			rl.cc.spec.initialWindowSize = s.Val
		case Http2SettingHeaderTableSize:
			rl.cc.henc.SetMaxDynamicTableSize(s.Val)
			rl.cc.fr.ReadMetaHeaders.SetMaxDynamicTableSize(s.Val)
		default:
		}
		return nil
	})
}

func (rl *http2clientConnReadLoop) processPing(f *Http2PingFrame) error {
	rl.cc.wmu.Lock()
	defer rl.cc.wmu.Unlock()
	if err := rl.cc.fr.WritePing(true, f.Data); err != nil {
		return err
	}
	return rl.cc.bw.Flush()
}
