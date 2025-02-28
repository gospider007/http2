package http2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/net/http2/hpack"
)

type errHttp2Code uint32

const (
	errHttp2CodeNo                 errHttp2Code = 0x0
	errHttp2CodeProtocol           errHttp2Code = 0x1
	errHttp2CodeInternal           errHttp2Code = 0x2
	errHttp2CodeFlowControl        errHttp2Code = 0x3
	errHttp2CodeSettingsTimeout    errHttp2Code = 0x4
	errHttp2CodeStreamClosed       errHttp2Code = 0x5
	errHttp2CodeFrameSize          errHttp2Code = 0x6
	errHttp2CodeRefusedStream      errHttp2Code = 0x7
	errHttp2CodeCancel             errHttp2Code = 0x8
	errHttp2CodeCompression        errHttp2Code = 0x9
	errHttp2CodeConnect            errHttp2Code = 0xa
	errHttp2CodeEnhanceYourCalm    errHttp2Code = 0xb
	errHttp2CodeInadequateSecurity errHttp2Code = 0xc
	errHttp2CodeHTTP11Required     errHttp2Code = 0xd
)

type http2ConnectionError errHttp2Code

func (e http2ConnectionError) Error() string {
	return fmt.Sprintf("connection error: %d", errHttp2Code(e))
}

// outflow is the outbound flow control window's size.
type http2outflow struct {

	// n is the number of DATA bytes we're allowed to send.
	// An outflow is kept both on a conn and a per-stream.
	n int32

	// conn points to the shared connection-level outflow that is
	// shared by all streams on that conn. It is nil for the outflow
	// that's on the conn directly.
	conn *http2outflow
}

func (f *http2outflow) setConnFlow(cf *http2outflow) { f.conn = cf }

func (f *http2outflow) available() int32 {
	n := f.n
	if f.conn != nil && f.conn.n < n {
		n = f.conn.n
	}
	return n
}

func (f *http2outflow) take(n int32) {
	// if n > f.available() {
	// 	panic("internal error: took too much")
	// }
	f.n -= n
	if f.conn != nil {
		f.conn.n -= n
	}
}

// add adds n bytes (positive or negative) to the flow control window.
// It returns false if the sum would exceed 2^31-1.
func (f *http2outflow) add(n int32) bool {
	sum := f.n + n
	if (sum > n) == (f.n > 0) {
		f.n = sum
		return true
	}
	return false
}

type http2inflow struct {
	avail  int32
	unsent int32
}

func (f *http2inflow) init(n int32) {
	f.avail = n
}
func (f *http2inflow) add(n int32) (connAdd int32) {
	unsent := int64(f.unsent) + int64(n)
	f.unsent = int32(unsent)
	if f.unsent < 4<<10 && f.unsent < f.avail {
		return 0
	}
	f.avail += f.unsent
	f.unsent = 0
	return int32(unsent)
}

const http2frameHeaderLen = 9

var http2padZeros = make([]byte, 255)

type http2FrameType uint8

const (
	http2FrameData         http2FrameType = 0x0
	http2FrameHeaders      http2FrameType = 0x1
	http2FramePriority     http2FrameType = 0x2
	http2FrameRSTStream    http2FrameType = 0x3
	http2FrameSettings     http2FrameType = 0x4
	http2FramePushPromise  http2FrameType = 0x5
	http2FramePing         http2FrameType = 0x6
	http2FrameGoAway       http2FrameType = 0x7
	http2FrameWindowUpdate http2FrameType = 0x8
	http2FrameContinuation http2FrameType = 0x9
)

type http2Flags uint8

func (f http2Flags) Has(v http2Flags) bool {
	return (f & v) == v
}

const (
	http2FlagDataEndStream http2Flags = 0x1
	http2FlagDataPadded    http2Flags = 0x8

	http2FlagHeadersEndStream  http2Flags = 0x1
	http2FlagHeadersEndHeaders http2Flags = 0x4
	http2FlagHeadersPadded     http2Flags = 0x8
	http2FlagHeadersPriority   http2Flags = 0x20

	http2FlagSettingsAck http2Flags = 0x1

	http2FlagPingAck http2Flags = 0x1

	http2FlagContinuationEndHeaders http2Flags = 0x4

	http2FlagPushPromiseEndHeaders http2Flags = 0x4
	http2FlagPushPromisePadded     http2Flags = 0x8
)

type http2frameParser func(fc *http2frameCache, fh http2FrameHeader, payload []byte) (any, error)

func http2parseHeadersFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (_ any, err error) {
	hf := &Http2HeadersFrame{
		http2FrameHeader: fh,
	}
	var padLength uint8
	if fh.Flags.Has(http2FlagHeadersPadded) {
		if p, padLength, err = http2readByte(p); err != nil {
			return
		}
	}
	if fh.Flags.Has(http2FlagHeadersPriority) {
		var v uint32
		p, v, err = http2readUint32(p)
		if err != nil {
			return nil, err
		}
		p, hf.Priority.Weight, err = http2readByte(p)
		if err != nil {
			return nil, err
		}
		hf.Priority.StreamDep = v & 0x7fffffff
		hf.Priority.Exclusive = (v != hf.Priority.StreamDep)
	}
	if len(p)-int(padLength) < 0 {
		return nil, errors.New("frame_headers_pad_too_big")
	}
	hf.headerFragBuf = p[:len(p)-int(padLength)]
	return hf, nil
}

var http2frameParsers = map[http2FrameType]http2frameParser{
	http2FrameData:         http2parseDataFrame,
	http2FrameHeaders:      http2parseHeadersFrame,
	http2FramePriority:     http2parsePriorityFrame,
	http2FrameRSTStream:    http2parseRSTStreamFrame,
	http2FrameSettings:     http2parseSettingsFrame,
	http2FramePushPromise:  http2parsePushPromise,
	http2FramePing:         http2parsePingFrame,
	http2FrameGoAway:       http2parseGoAwayFrame,
	http2FrameWindowUpdate: http2parseWindowUpdateFrame,
	http2FrameContinuation: http2parseContinuationFrame,
}

func http2typeFrameParser(t http2FrameType) http2frameParser {
	if f := http2frameParsers[t]; f != nil {
		return f
	}
	return http2parseUnknownFrame
}

type http2FrameHeader struct {
	Type     http2FrameType
	Flags    http2Flags
	Length   uint32
	StreamID uint32
}

func http2readFrameHeader(buf []byte, r io.Reader) (http2FrameHeader, error) {
	_, err := io.ReadFull(r, buf[:http2frameHeaderLen])
	if err != nil {
		return http2FrameHeader{}, err
	}
	return http2FrameHeader{
		Length:   (uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])),
		Type:     http2FrameType(buf[3]),
		Flags:    http2Flags(buf[4]),
		StreamID: binary.BigEndian.Uint32(buf[5:]) & (1<<31 - 1),
	}, nil
}

type http2Framer struct {
	r               io.Reader
	w               io.Writer
	getReadBuf      func(size uint32) []byte
	ReadMetaHeaders *hpack.Decoder
	frameCache      *http2frameCache
	readBuf         []byte
	wbuf            []byte
	headerBuf       [http2frameHeaderLen]byte
}

func (f *http2Framer) startWrite(ftype http2FrameType, flags http2Flags, streamID uint32) {
	f.wbuf = append(f.wbuf[:0],
		0,
		0,
		0,
		byte(ftype),
		byte(flags),
		byte(streamID>>24),
		byte(streamID>>16),
		byte(streamID>>8),
		byte(streamID))
}

func (f *http2Framer) endWrite() error {
	length := len(f.wbuf) - http2frameHeaderLen
	if length >= (1 << 24) {
		return errors.New("http2: frame too large")
	}
	_ = append(f.wbuf[:0],
		byte(length>>16),
		byte(length>>8),
		byte(length))
	n, err := f.w.Write(f.wbuf)
	if err == nil && n != len(f.wbuf) {
		err = io.ErrShortWrite
	}
	return err
}

func (f *http2Framer) writeByte(v byte) { f.wbuf = append(f.wbuf, v) }

func (f *http2Framer) writeBytes(v []byte) { f.wbuf = append(f.wbuf, v...) }

func (f *http2Framer) writeUint16(v uint16) { f.wbuf = append(f.wbuf, byte(v>>8), byte(v)) }

func (f *http2Framer) writeUint32(v uint32) {
	f.wbuf = append(f.wbuf, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

type http2frameCache struct {
	dataFrame Http2DataFrame
}

func (fc *http2frameCache) getDataFrame() *Http2DataFrame {
	if fc == nil {
		return &Http2DataFrame{}
	}
	return &fc.dataFrame
}

func http2NewFramer(w io.Writer, r io.Reader) *http2Framer {
	fr := &http2Framer{
		w: w,
		r: r,
	}
	fr.getReadBuf = func(size uint32) []byte {
		if cap(fr.readBuf) >= int(size) {
			return fr.readBuf[:size]
		}
		fr.readBuf = make([]byte, size)
		return fr.readBuf
	}
	return fr
}

func (fr *http2Framer) ReadFrame() (any, error) {
	fh, err := http2readFrameHeader(fr.headerBuf[:], fr.r)
	if err != nil {
		return nil, err
	}
	payload := fr.getReadBuf(fh.Length)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		return nil, err
	}
	f, err := http2typeFrameParser(fh.Type)(fr.frameCache, fh, payload)
	if err != nil {
		return nil, err
	}
	if fh.Type == http2FrameHeaders {
		return fr.readMetaFrame(f.(*Http2HeadersFrame))
	}
	return f, nil
}
func (fr *http2Framer) readMetaFrame(hf *Http2HeadersFrame) (any, error) {
	mh := &Http2MetaHeadersFrame{
		Http2HeadersFrame: hf,
	}
	fr.ReadMetaHeaders.SetEmitEnabled(true)
	fr.ReadMetaHeaders.SetEmitFunc(func(hf hpack.HeaderField) {
		mh.Fields = append(mh.Fields, hf)
	})
	defer func() {
		fr.ReadMetaHeaders.SetEmitEnabled(false)
		fr.ReadMetaHeaders.SetEmitFunc(nil)
	}()
	var hc http2headersOrContinuation = hf
	for {
		frag := hc.HeaderBlockFragment()
		if _, err := fr.ReadMetaHeaders.Write(frag); err != nil {
			return mh, http2ConnectionError(errHttp2CodeCompression)
		}
		if hc.HeadersEnded() {
			break
		}
		if f, err := fr.ReadFrame(); err != nil {
			return nil, err
		} else {
			hc = f.(*http2ContinuationFrame)
		}
	}
	mh.Http2HeadersFrame.headerFragBuf = nil
	if err := fr.ReadMetaHeaders.Close(); err != nil {
		return mh, http2ConnectionError(errHttp2CodeCompression)
	}
	return mh, nil
}

type Http2DataFrame struct {
	data []byte
	http2FrameHeader
}

func (f *Http2DataFrame) StreamEnded() bool {
	return f.http2FrameHeader.Flags.Has(http2FlagDataEndStream)
}

func (f *Http2DataFrame) Data() []byte {
	return f.data
}

func http2parseDataFrame(fc *http2frameCache, fh http2FrameHeader, payload []byte) (any, error) {
	if fh.StreamID == 0 {
		return nil, errors.New("DATA frame with stream ID 0")
	}
	f := fc.getDataFrame()
	f.http2FrameHeader = fh

	var padSize byte
	if fh.Flags.Has(http2FlagDataPadded) {
		var err error
		payload, padSize, err = http2readByte(payload)
		if err != nil {
			return nil, err
		}
	}
	if int(padSize) > len(payload) {
		return nil, errors.New("pad size larger than data payload")
	}
	f.data = payload[:len(payload)-int(padSize)]
	return f, nil
}

var (
	errHttp2PadLength = errors.New("pad length too large")
)

func (f *http2Framer) WriteData(streamID uint32, endStream bool, data []byte) error {
	return f.WriteDataPadded(streamID, endStream, data, nil)
}

func (f *http2Framer) WriteDataPadded(streamID uint32, endStream bool, data, pad []byte) error {
	if err := f.startWriteDataPadded(streamID, endStream, data, pad); err != nil {
		return err
	}
	return f.endWrite()
}

func (f *http2Framer) startWriteDataPadded(streamID uint32, endStream bool, data, pad []byte) error {
	if len(pad) > 255 {
		return errHttp2PadLength
	}
	var flags http2Flags
	if endStream {
		flags |= http2FlagDataEndStream
	}
	if pad != nil {
		flags |= http2FlagDataPadded
	}
	f.startWrite(http2FrameData, flags, streamID)
	if pad != nil {
		f.wbuf = append(f.wbuf, byte(len(pad)))
	}
	f.wbuf = append(f.wbuf, data...)
	f.wbuf = append(f.wbuf, pad...)
	return nil
}

type Http2SettingsFrame struct {
	p []byte
	http2FrameHeader
}

func http2parseSettingsFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (any, error) {
	if fh.Flags.Has(http2FlagSettingsAck) && fh.Length > 0 {
		return nil, http2ConnectionError(errHttp2CodeFrameSize)
	}
	if fh.StreamID != 0 {
		return nil, http2ConnectionError(errHttp2CodeProtocol)
	}
	if len(p)%6 != 0 {
		return nil, http2ConnectionError(errHttp2CodeFrameSize)
	}
	f := &Http2SettingsFrame{http2FrameHeader: fh, p: p}
	if v, ok := f.Value(Http2SettingInitialWindowSize); ok && v > (1<<31)-1 {
		return nil, http2ConnectionError(errHttp2CodeFlowControl)
	}
	return f, nil
}

func (f *Http2SettingsFrame) IsAck() bool {
	return f.http2FrameHeader.Flags.Has(http2FlagSettingsAck)
}

func (f *Http2SettingsFrame) Value(id Http2SettingID) (v uint32, ok bool) {
	for i := 0; i < f.NumSettings(); i++ {
		if s := f.Setting(i); s.ID == id {
			return s.Val, true
		}
	}
	return 0, false
}

func (f *Http2SettingsFrame) Setting(i int) Http2Setting {
	buf := f.p
	return Http2Setting{
		ID:  Http2SettingID(binary.BigEndian.Uint16(buf[i*6 : i*6+2])),
		Val: binary.BigEndian.Uint32(buf[i*6+2 : i*6+6]),
	}
}

func (f *Http2SettingsFrame) NumSettings() int { return len(f.p) / 6 }

func (f *Http2SettingsFrame) HasDuplicates() bool {
	num := f.NumSettings()
	if num == 0 {
		return false
	}

	if num < 10 {
		for i := 0; i < num; i++ {
			idi := f.Setting(i).ID
			for j := i + 1; j < num; j++ {
				idj := f.Setting(j).ID
				if idi == idj {
					return true
				}
			}
		}
		return false
	}
	seen := map[Http2SettingID]bool{}
	for i := 0; i < num; i++ {
		id := f.Setting(i).ID
		if seen[id] {
			return true
		}
		seen[id] = true
	}
	return false
}

func (f *Http2SettingsFrame) ForeachSetting(fn func(Http2Setting) error) error {
	for i := 0; i < f.NumSettings(); i++ {
		if err := fn(f.Setting(i)); err != nil {
			return err
		}
	}
	return nil
}

func (f *http2Framer) WriteSettings(settings ...Http2Setting) error {
	f.startWrite(http2FrameSettings, 0, 0)
	for _, s := range settings {
		f.writeUint16(uint16(s.ID))
		f.writeUint32(s.Val)
	}
	return f.endWrite()
}

func (f *http2Framer) WriteSettingsAck() error {
	f.startWrite(http2FrameSettings, http2FlagSettingsAck, 0)
	return f.endWrite()
}

type Http2PingFrame struct {
	http2FrameHeader
	Data [8]byte
}

func (f *Http2PingFrame) IsAck() bool { return f.Flags.Has(http2FlagPingAck) }

func http2parsePingFrame(_ *http2frameCache, fh http2FrameHeader, payload []byte) (any, error) {
	if len(payload) != 8 {
		return nil, http2ConnectionError(errHttp2CodeFrameSize)
	}
	if fh.StreamID != 0 {
		return nil, http2ConnectionError(errHttp2CodeProtocol)
	}
	f := &Http2PingFrame{http2FrameHeader: fh}
	copy(f.Data[:], payload)
	return f, nil
}

func (f *http2Framer) WritePing(ack bool, data [8]byte) error {
	var flags http2Flags
	if ack {
		flags = http2FlagPingAck
	}
	f.startWrite(http2FramePing, flags, 0)
	f.writeBytes(data[:])
	return f.endWrite()
}

type Http2GoAwayFrame struct {
	debugData []byte
	http2FrameHeader
	LastStreamID uint32
	ErrCode      errHttp2Code
}

func http2parseGoAwayFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (any, error) {
	if fh.StreamID != 0 {
		return nil, http2ConnectionError(errHttp2CodeProtocol)
	}
	if len(p) < 8 {
		return nil, http2ConnectionError(errHttp2CodeFrameSize)
	}
	return &Http2GoAwayFrame{
		http2FrameHeader: fh,
		LastStreamID:     binary.BigEndian.Uint32(p[:4]) & (1<<31 - 1),
		ErrCode:          errHttp2Code(binary.BigEndian.Uint32(p[4:8])),
		debugData:        p[8:],
	}, nil
}

type http2UnknownFrame struct {
	p []byte
	http2FrameHeader
}

func (f *http2UnknownFrame) Payload() []byte {
	return f.p
}

func http2parseUnknownFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (any, error) {
	return &http2UnknownFrame{p, fh}, nil
}

type Http2WindowUpdateFrame struct {
	http2FrameHeader
	Increment uint32
}

func http2parseWindowUpdateFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (any, error) {
	if len(p) != 4 {
		return nil, http2ConnectionError(errHttp2CodeFrameSize)
	}
	return &Http2WindowUpdateFrame{
		http2FrameHeader: fh,
		Increment:        binary.BigEndian.Uint32(p[:4]) & 0x7fffffff,
	}, nil
}

func (f *http2Framer) WriteWindowUpdate(streamID, incr uint32) error {
	f.startWrite(http2FrameWindowUpdate, 0, streamID)
	f.writeUint32(incr)
	return f.endWrite()
}

type Http2HeadersFrame struct {
	headerFragBuf []byte
	http2FrameHeader
	Priority Http2PriorityParam
}

func (f *Http2HeadersFrame) HeaderBlockFragment() []byte {
	return f.headerFragBuf
}

func (f *Http2HeadersFrame) HeadersEnded() bool {
	return f.http2FrameHeader.Flags.Has(http2FlagHeadersEndHeaders)
}

func (f *Http2HeadersFrame) StreamEnded() bool {
	return f.http2FrameHeader.Flags.Has(http2FlagHeadersEndStream)
}

type Http2HeadersFrameParam struct {
	BlockFragment []byte
	Priority      Http2PriorityParam
	StreamID      uint32
	EndStream     bool
	EndHeaders    bool
	PadLength     uint8
}

func (f *http2Framer) WriteHeaders(p Http2HeadersFrameParam) error {
	var flags http2Flags
	if p.PadLength != 0 {
		flags |= http2FlagHeadersPadded
	}
	if p.EndStream {
		flags |= http2FlagHeadersEndStream
	}
	if p.EndHeaders {
		flags |= http2FlagHeadersEndHeaders
	}
	if !p.Priority.IsZero() {
		flags |= http2FlagHeadersPriority
	}
	f.startWrite(http2FrameHeaders, flags, p.StreamID)
	if p.PadLength != 0 {
		f.writeByte(p.PadLength)
	}
	if !p.Priority.IsZero() {
		v := p.Priority.StreamDep
		if p.Priority.Exclusive {
			v |= 1 << 31
		}
		f.writeUint32(v)
		f.writeByte(p.Priority.Weight)
	}
	f.wbuf = append(f.wbuf, p.BlockFragment...)
	f.wbuf = append(f.wbuf, http2padZeros[:p.PadLength]...)
	return f.endWrite()
}

type http2PriorityFrame struct {
	http2FrameHeader
	Http2PriorityParam
}

type Http2PriorityParam struct {
	StreamDep uint32

	Exclusive bool

	Weight uint8
}

func (p Http2PriorityParam) IsZero() bool {
	return p == Http2PriorityParam{}
}

func http2parsePriorityFrame(_ *http2frameCache, fh http2FrameHeader, payload []byte) (any, error) {
	if fh.StreamID == 0 {
		return nil, errors.New("PRIORITY frame with stream ID 0")
	}
	if len(payload) != 5 {
		return nil, fmt.Errorf("PRIORITY frame payload size was %d; want 5", len(payload))
	}
	v := binary.BigEndian.Uint32(payload[:4])
	streamID := v & 0x7fffffff
	return &http2PriorityFrame{
		http2FrameHeader: fh,
		Http2PriorityParam: Http2PriorityParam{
			Weight:    payload[4],
			StreamDep: streamID,
			Exclusive: streamID != v,
		},
	}, nil
}

func (f *http2Framer) WritePriority(streamID uint32, p Http2PriorityParam) error {
	f.startWrite(http2FramePriority, 0, streamID)
	v := p.StreamDep
	if p.Exclusive {
		v |= 1 << 31
	}
	f.writeUint32(v)
	f.writeByte(p.Weight)
	return f.endWrite()
}

type Http2RSTStreamFrame struct {
	http2FrameHeader
	ErrCode errHttp2Code
}

func http2parseRSTStreamFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (any, error) {
	if len(p) != 4 {
		return nil, http2ConnectionError(errHttp2CodeFrameSize)
	}
	if fh.StreamID == 0 {
		return nil, http2ConnectionError(errHttp2CodeProtocol)
	}
	return &Http2RSTStreamFrame{fh, errHttp2Code(binary.BigEndian.Uint32(p[:4]))}, nil
}

func (f *http2Framer) WriteRSTStream(streamID uint32, code errHttp2Code) error {
	f.startWrite(http2FrameRSTStream, 0, streamID)
	f.writeUint32(uint32(code))
	return f.endWrite()
}

type http2ContinuationFrame struct {
	headerFragBuf []byte
	http2FrameHeader
}

func http2parseContinuationFrame(_ *http2frameCache, fh http2FrameHeader, p []byte) (any, error) {
	if fh.StreamID == 0 {
		return nil, errors.New("CONTINUATION frame with stream ID 0")
	}
	return &http2ContinuationFrame{p, fh}, nil
}

func (f *http2ContinuationFrame) HeaderBlockFragment() []byte {
	return f.headerFragBuf
}

func (f *http2ContinuationFrame) HeadersEnded() bool {
	return f.http2FrameHeader.Flags.Has(http2FlagContinuationEndHeaders)
}

func (f *http2Framer) WriteContinuation(streamID uint32, endHeaders bool, headerBlockFragment []byte) error {
	var flags http2Flags
	if endHeaders {
		flags |= http2FlagContinuationEndHeaders
	}
	f.startWrite(http2FrameContinuation, flags, streamID)
	f.wbuf = append(f.wbuf, headerBlockFragment...)
	return f.endWrite()
}

type Http2PushPromiseFrame struct {
	headerFragBuf []byte
	http2FrameHeader
	PromiseID uint32
}

func (f *Http2PushPromiseFrame) HeaderBlockFragment() []byte {
	return f.headerFragBuf
}

func (f *Http2PushPromiseFrame) HeadersEnded() bool {
	return f.http2FrameHeader.Flags.Has(http2FlagPushPromiseEndHeaders)
}

func http2parsePushPromise(_ *http2frameCache, fh http2FrameHeader, p []byte) (_ any, err error) {
	pp := &Http2PushPromiseFrame{
		http2FrameHeader: fh,
	}
	if pp.StreamID == 0 {
		return nil, http2ConnectionError(errHttp2CodeProtocol)
	}
	var padLength uint8
	if fh.Flags.Has(http2FlagPushPromisePadded) {
		if p, padLength, err = http2readByte(p); err != nil {
			return
		}
	}
	p, pp.PromiseID, err = http2readUint32(p)
	if err != nil {
		return
	}
	pp.PromiseID = pp.PromiseID & (1<<31 - 1)

	if int(padLength) > len(p) {

		return nil, http2ConnectionError(errHttp2CodeProtocol)
	}
	pp.headerFragBuf = p[:len(p)-int(padLength)]
	return pp, nil
}

type http2PushPromiseParam struct {
	BlockFragment []byte

	StreamID uint32

	PromiseID uint32

	EndHeaders bool

	PadLength uint8
}

func (f *http2Framer) WritePushPromise(p http2PushPromiseParam) error {
	var flags http2Flags
	if p.PadLength != 0 {
		flags |= http2FlagPushPromisePadded
	}
	if p.EndHeaders {
		flags |= http2FlagPushPromiseEndHeaders
	}
	f.startWrite(http2FramePushPromise, flags, p.StreamID)
	if p.PadLength != 0 {
		f.writeByte(p.PadLength)
	}
	f.writeUint32(p.PromiseID)
	f.wbuf = append(f.wbuf, p.BlockFragment...)
	f.wbuf = append(f.wbuf, http2padZeros[:p.PadLength]...)
	return f.endWrite()
}

func (f *http2Framer) WriteRawFrame(t http2FrameType, flags http2Flags, streamID uint32, payload []byte) error {
	f.startWrite(t, flags, streamID)
	f.writeBytes(payload)
	return f.endWrite()
}

func http2readByte(p []byte) (remain []byte, b byte, err error) {
	if len(p) == 0 {
		return nil, 0, io.ErrUnexpectedEOF
	}
	return p[1:], p[0], nil
}

func http2readUint32(p []byte) (remain []byte, v uint32, err error) {
	if len(p) < 4 {
		return nil, 0, io.ErrUnexpectedEOF
	}
	return p[4:], binary.BigEndian.Uint32(p[:4]), nil
}

type http2headersEnder interface {
	HeadersEnded() bool
}

type http2headersOrContinuation interface {
	http2headersEnder
	HeaderBlockFragment() []byte
}

type Http2MetaHeadersFrame struct {
	*Http2HeadersFrame
	Fields []hpack.HeaderField
}

func (mh *Http2MetaHeadersFrame) PseudoValue(pseudo string) string {
	for _, hf := range mh.Fields {
		if !hf.IsPseudo() {
			return ""
		}
		if hf.Name[1:] == pseudo {
			return hf.Value
		}
	}
	return ""
}

func (mh *Http2MetaHeadersFrame) RegularFields() []hpack.HeaderField {
	for i, hf := range mh.Fields {
		if !hf.IsPseudo() {
			return mh.Fields[i:]
		}
	}
	return nil
}

func (mh *Http2MetaHeadersFrame) PseudoFields() []hpack.HeaderField {
	for i, hf := range mh.Fields {
		if !hf.IsPseudo() {
			return mh.Fields[:i]
		}
	}
	return mh.Fields
}

type Http2Setting struct {
	ID Http2SettingID

	Val uint32
}

type Http2SettingID uint16

const (
	Http2SettingHeaderTableSize   Http2SettingID = 0x1
	Http2SettingInitialWindowSize Http2SettingID = 0x4
	Http2SettingMaxFrameSize      Http2SettingID = 0x5
	Http2SettingMaxHeaderListSize Http2SettingID = 0x6
)

func http2validPseudoPath(v string) bool {
	return (len(v) > 0 && v[0] == '/') || v == "*"
}
