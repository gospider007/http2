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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gospider007/ja3"
	"github.com/gospider007/tools"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2/hpack"
)

type Http2ClientConn struct {
	tconn             net.Conn
	closeFunc         func()
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
	closeCtx    context.Context
	closeCnl    context.CancelCauseFunc
}

func (cc *Http2ClientConn) CloseCtx() context.Context {
	return cc.closeCtx
}
func (obj *Http2ClientConn) Stream() io.ReadWriteCloser {
	return nil
}

type http2clientStream struct {
	cc         *Http2ClientConn
	resp       *http.Response
	req        *http.Request
	bodyReader *io.PipeReader
	bodyWriter *io.PipeWriter
	ID         uint32
	inflow     http2inflow
	flow       http2outflow

	err error
	ctx context.Context
	cnl context.CancelFunc

	writeCtx context.Context
	writeCnl context.CancelFunc

	readCtx context.Context
	readCnl context.CancelCauseFunc
}

func (cc *Http2ClientConn) notice() {
	select {
	case cc.flowNotices <- struct{}{}:
	default:
	}
}
func (cc *Http2ClientConn) run() (err error) {
	defer cc.CloseWithError(err)
	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			return tools.WrapError(err, "ReadFrame")
		}
		switch f := f.(type) {
		case *http2MetaHeadersFrame:
			if cc.http2clientStream == nil {
				return tools.WrapError(errors.New("unexpected meta headers frame"), "run")
			}
			cc.http2clientStream.resp, err = cc.loop.handleResponse(cc.http2clientStream, f)
			cc.http2clientStream.cnl()
			if err != nil {
				return tools.WrapError(err, "handleResponse")
			}
		case *http2DataFrame:
			if err = cc.loop.processData(cc.http2clientStream, f); err != nil {
				return tools.WrapError(err, "processData")
			}
		case *http2GoAwayFrame:
			if f.ErrCode == 0 {
				err = fmt.Errorf("http2: server sent GOAWAY with close connection ok")
			} else {
				err = fmt.Errorf("http2: server sent GOAWAY with error code %v", f.ErrCode)
			}
			return tools.WrapError(err, "processGoAway")
		case *http2RSTStreamFrame:
			if f.ErrCode == 0 {
				err = fmt.Errorf("http2: server sent processResetStream with close connection ok")
			} else {
				err = fmt.Errorf("http2: server sent processResetStream with error code %v", f.ErrCode)
			}
			return tools.WrapError(err, "processResetStream")
		case *http2SettingsFrame:
			if err = cc.loop.processSettings(f); err != nil {
				return tools.WrapError(err, "processSettings")
			}
		case *http2PushPromiseFrame:
			err = http2ConnectionError(errHttp2CodeProtocol)
			return tools.WrapError(err, "processPushPromise")
		case *http2WindowUpdateFrame:
			if err = cc.loop.processWindowUpdate(f); err != nil {
				return tools.WrapError(err, "processWindowUpdate")
			}
		case *http2PingFrame:
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
	initialSetting    []ja3.Setting
	priority          ja3.Priority
	connFlow          uint32
	initialWindowSize uint32
	headerTableSize   uint32
	maxHeaderListSize uint32
	maxFrameSize      uint32
}

func spec2option(h2Spec ja3.HSpec) (option gospiderOption) {
	option.initialSetting = h2Spec.InitialSetting
	option.priority = ja3.Priority{
		Exclusive: true,
		StreamDep: 0,
		Weight:    255,
	}
	option.headerTableSize = 65536
	option.maxHeaderListSize = 262144
	option.initialWindowSize = 6291456
	option.maxFrameSize = 16384
	option.connFlow = 15663105

	// //golang setting: start
	// option.initialWindowSize = 4194304
	// option.maxFrameSize = 16384
	// option.maxHeaderListSize = 10485760
	// option.initialSetting = []ja3.Setting{
	// 	{Id: 2, Val: 0},
	// 	{Id: 4, Val: option.initialWindowSize},
	// 	{Id: 5, Val: option.maxFrameSize},
	// 	{Id: 6, Val: option.maxHeaderListSize},
	// }
	// option.priority = ja3.Priority{}
	// option.connFlow = 1073741824
	// option.headerTableSize = 4096
	// //golang setting: end

	if len(option.initialSetting) > 0 {
		for _, setting := range option.initialSetting {
			switch setting.Id {
			case ja3.Http2SettingHeaderTableSize:
				option.headerTableSize = setting.Val
			case ja3.Http2SettingMaxHeaderListSize:
				option.maxHeaderListSize = setting.Val
			case ja3.Http2SettingInitialWindowSize:
				option.initialWindowSize = setting.Val
			case ja3.Http2SettingMaxFrameSize:
				option.maxFrameSize = setting.Val
			}
		}
	} else {
		option.initialSetting = []ja3.Setting{
			{Id: ja3.Http2SettingHeaderTableSize, Val: option.headerTableSize},
			{Id: ja3.Http2SettingEnablePush, Val: 0},
			{Id: ja3.Http2SettingInitialWindowSize, Val: option.initialWindowSize},
			{Id: ja3.Http2SettingMaxHeaderListSize, Val: option.maxHeaderListSize},
		}
	}
	return option
}

func NewClientConn(ctx context.Context, c net.Conn, h2Spec ja3.HSpec, closefun func()) (*Http2ClientConn, error) {
	spec := spec2option(h2Spec)
	cc := &Http2ClientConn{
		closeFunc:   closefun,
		spec:        spec,
		tconn:       c,
		flowNotices: make(chan struct{}, 1),
	}
	cc.closeCtx, cc.closeCnl = context.WithCancelCause(context.TODO())
	cc.bw = bufio.NewWriter(c)
	cc.fr = http2NewFramer(cc.bw, bufio.NewReader(c))
	cc.fr.ReadMetaHeaders = hpack.NewDecoder(cc.spec.headerTableSize, nil)
	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.henc.SetMaxDynamicTableSizeLimit(cc.spec.headerTableSize)
	initialSettings := make([]http2Setting, len(cc.spec.initialSetting))
	for i, setting := range cc.spec.initialSetting {
		initialSettings[i] = http2Setting{ID: http2SettingID(setting.Id), Val: setting.Val}
	}
	cc.spec.initialWindowSize = 65535
	cc.flow.add(int32(cc.spec.initialWindowSize))
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		if _, err = cc.bw.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")); err != nil {
			return
		}
		if err = cc.fr.WriteSettings(initialSettings...); err != nil {
			return
		}
		if err = cc.fr.WriteWindowUpdate(0, cc.spec.connFlow); err != nil {
			return
		}
		cc.inflow.init(int32(cc.spec.connFlow) + int32(cc.spec.initialWindowSize))
		if err = cc.bw.Flush(); err != nil {
			return
		}
	}()
	select {
	case <-done:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	cc.loop = &http2clientConnReadLoop{cc: cc}
	go cc.run()
	return cc, nil
}

func (cc *Http2ClientConn) CloseWithError(err error) error {
	cc.closeCnl(err)
	if err != io.EOF && cc.closeFunc != nil {
		cc.closeFunc()
	}
	cc.tconn.Close()
	if cc.http2clientStream != nil {
		cc.http2clientStream.cnl()
		cc.http2clientStream.bodyWriter.CloseWithError(err)
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

func (cc *Http2ClientConn) initStream(req *http.Request) {
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	if cc.streamID == 0 {
		cc.streamID = 1
	} else {
		cc.streamID += 2
	}
	reader, writer := io.Pipe()
	cs := &http2clientStream{
		cc:         cc,
		bodyReader: reader,
		bodyWriter: writer,
		req:        req,
	}
	cs.writeCtx, cs.writeCnl = context.WithCancel(cc.closeCtx)
	cs.readCtx, cs.readCnl = context.WithCancelCause(cc.closeCtx)
	cs.ctx, cs.cnl = context.WithCancel(req.Context())

	cc.http2clientStream = cs
	cs.inflow.init(int32(cs.cc.spec.initialWindowSize))
	cs.flow.add(int32(cs.cc.spec.initialWindowSize))
	cs.flow.setConnFlow(&cs.cc.flow)
	cs.ID = cs.cc.streamID
}

func (cc *Http2ClientConn) DoRequest(req *http.Request, orderHeaders []string) (response *http.Response, bodyCtx context.Context, err error) {
	defer func() {
		if err != nil {
			cc.CloseWithError(err)
		}
	}()
	cc.initStream(req)
	go cc.http2clientStream.writeRequest(req, orderHeaders)
	select {
	case <-cc.CloseCtx().Done():
		return cc.http2clientStream.resp, cc.http2clientStream.readCtx, cc.http2clientStream.err
	case <-cc.http2clientStream.ctx.Done():
		return cc.http2clientStream.resp, cc.http2clientStream.readCtx, cc.http2clientStream.err
	case <-req.Context().Done():
		return nil, nil, req.Context().Err()
	}
}

func (cs *http2clientStream) writeRequest(req *http.Request, orderHeaders []string) (err error) {
	defer func() {
		if err != nil {
			cs.cc.CloseWithError(err)
		}
		cs.writeCnl()
	}()
	if err = cs.encodeAndWriteHeaders(req, orderHeaders); err != nil {
		return err
	}
	if http2actualContentLength(cs.req) != 0 {
		return cs.writeRequestBody(req)
	}
	return
}

func (cs *http2clientStream) encodeAndWriteHeaders(req *http.Request, orderHeaders []string) error {
	cs.cc.wmu.Lock()
	defer cs.cc.wmu.Unlock()
	hdrs, err := cs.cc.encodeHeaders(req, orderHeaders)
	if err != nil {
		return err
	}
	return cs.cc.writeHeaders(cs.ID, http2actualContentLength(req) == 0, int(cs.cc.spec.maxFrameSize), hdrs)
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
			http2HeadersFrameParam := http2HeadersFrameParam{
				StreamID:      streamID,
				BlockFragment: chunk,
				EndStream:     endStream,
				EndHeaders:    endHeaders,
			}
			if cc.spec.priority.StreamDep != 0 || cc.spec.priority.Weight != 0 || cc.spec.priority.Exclusive {
				http2HeadersFrameParam.Priority = http2PriorityParam{
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

func (cs *http2clientStream) frameScratchBufferLen(maxFrameSize int) int {
	const max = 512 << 10
	n := int64(maxFrameSize)
	if n > max {
		n = max
	}
	if cl := http2actualContentLength(cs.req); cl != -1 && cl+1 < n {
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
		case <-cs.cc.closeCtx.Done():
			return 0, context.Cause(cs.cc.closeCtx)
		case <-cs.readCtx.Done():
			return 0, context.Cause(cs.readCtx)
		case <-cs.cc.flowNotices:
		case <-time.After(time.Second * 30):
			return 0, errors.New("timeout waiting for flow control")
		}
	}
}

func (cs *http2clientStream) writeRequestBody(req *http.Request) (bodyErr error) {
	buf := make([]byte, cs.frameScratchBufferLen(int(cs.cc.spec.maxFrameSize)))
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

func (cc *Http2ClientConn) encodeHeaders(req *http.Request, orderHeaders []string) ([]byte, error) {
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
		f(":authority", host)
		f(":method", req.Method)
		if req.Method != http.MethodConnect {
			f(":path", path)
			f(":scheme", req.URL.Scheme)
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
		if contentLength := http2actualContentLength(req); http2shouldSendReqContentLength(req.Method, contentLength) {
			f("content-length", strconv.FormatInt(contentLength, 10))
		}
		sort.Slice(gospiderHeaders, func(x, y int) bool {
			xI := slices.Index(orderHeaders, gospiderHeaders[x][0])
			yI := slices.Index(orderHeaders, gospiderHeaders[y][0])
			if xI < 0 {
				return false
			}
			if yI < 0 {
				return true
			}
			if xI <= yI {
				return true
			}
			return false
		})
		for _, kv := range gospiderHeaders {
			replaceF(kv[0], kv[1])
		}
	}
	hlSize := uint64(0)
	enumerateHeaders(func(name, value string) {
		hf := hpack.HeaderField{Name: name, Value: value}
		hlSize += uint64(hf.Size())
	})
	enumerateHeaders(func(name, value string) {
		name = strings.ToLower(name)
		cc.writeHeader(name, value)
	})
	return cc.hbuf.Bytes(), nil
}

func http2shouldSendReqContentLength(method string, contentLength int64) bool {
	if contentLength > 0 {
		return true
	}
	if contentLength < 0 {
		return false
	}

	switch method {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}

func (cc *Http2ClientConn) writeHeader(name, value string) {
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

type http2clientConnReadLoop struct {
	cc *Http2ClientConn
}

type http2noBodyReader struct{}

func (http2noBodyReader) Close() error { return nil }

func (http2noBodyReader) Read([]byte) (int, error) { return 0, io.EOF }

func (rl *http2clientConnReadLoop) handleResponse(cs *http2clientStream, f *http2MetaHeadersFrame) (*http.Response, error) {
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
		Request:    cs.req,
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
		res.Body = http2noBodyReader{}
		return res, nil
	}
	res.Body = http2transportResponseBody{cs}
	return res, nil
}

type http2transportResponseBody struct {
	cs *http2clientStream
}

func (b http2transportResponseBody) Read(p []byte) (n int, err error) {
	return b.cs.bodyReader.Read(p)
}
func (b http2transportResponseBody) Close() error {
	return b.cs.bodyReader.Close()
}
func (rl *http2clientConnReadLoop) processData(cs *http2clientStream, f *http2DataFrame) (err error) {
	if f.Length > 0 {
		if len(f.Data()) > 0 {
			if _, err = cs.bodyWriter.Write(f.Data()); err != nil {
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
		select {
		case <-cs.writeCtx.Done():
		default:
			err = errors.New("last task not write done with read done")
		}
		cs.readCnl(err)
		cs.bodyWriter.CloseWithError(io.EOF)
	}
	return
}

func (rl *http2clientConnReadLoop) processWindowUpdate(f *http2WindowUpdateFrame) error {
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
func (rl *http2clientConnReadLoop) processSettings(f *http2SettingsFrame) error {
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
func (rl *http2clientConnReadLoop) processSettingsNoWrite(f *http2SettingsFrame) error {
	return f.ForeachSetting(func(s http2Setting) error {
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
		default:
		}
		return nil
	})
}

func (rl *http2clientConnReadLoop) processPing(f *http2PingFrame) error {
	rl.cc.wmu.Lock()
	defer rl.cc.wmu.Unlock()
	if err := rl.cc.fr.WritePing(true, f.Data); err != nil {
		return err
	}
	return rl.cc.bw.Flush()
}
