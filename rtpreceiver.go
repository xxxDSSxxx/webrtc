// +build !js

package webrtc

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/srtp"
)

const defaultRid = "default-rid"

// RTPReceiver allows an application to inspect the receipt of a Track
type RTPReceiver struct {
	kind      RTPCodecType
	transport *DTLSTransport

	track  *Track
	tracks map[string]*Track

	closed, initialized chan interface{}
	mu                  sync.RWMutex

	rtpReadStream  *srtp.ReadStreamSRTP
	rtcpReadStream *srtp.ReadStreamSRTCP

	rids   map[string]string
	useRid bool

	rtpReadStreams       map[string]*srtp.ReadStreamSRTP
	rtcpReadStreams      map[string]*srtp.ReadStreamSRTCP
	rtpReadStreamsReady  map[string]chan struct{}
	rtcpReadStreamsReady map[string]chan struct{}

	// A reference to the associated api object
	api *API
}

// NewRTPReceiver constructs a new RTPReceiver
func (api *API) NewRTPReceiver(kind RTPCodecType, transport *DTLSTransport) (*RTPReceiver, error) {
	if transport == nil {
		return nil, fmt.Errorf("DTLSTransport must not be nil")
	}

	return &RTPReceiver{
		kind:        kind,
		transport:   transport,
		api:         api,
		closed:      make(chan interface{}),
		initialized: make(chan interface{}),
	}, nil
}

func (r *RTPReceiver) readyStreams() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	c := 0
	for _, s := range r.rtpReadStreams {
		if s != nil {
			c++
		}
	}
	return c
}

// Transport returns the currently-configured *DTLSTransport or nil
// if one has not yet been configured
func (r *RTPReceiver) Transport() *DTLSTransport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.transport
}

// Track returns the RTCRtpTransceiver track
func (r *RTPReceiver) Track() *Track {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.track
}

// Tracks returns the RTCRtpTransceiver track
func (r *RTPReceiver) Tracks() map[string]*Track {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tracks
}

// Receive initialize the track and starts all the transports
func (r *RTPReceiver) Receive(parameters RTPReceiveParameters) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(parameters.Encodings) == 0 {
		return fmt.Errorf("no encodings provided")
	}

	select {
	case <-r.initialized:
		return fmt.Errorf("Receive has already been called")
	default:
	}
	defer close(r.initialized)

	r.rtpReadStreams = make(map[string]*srtp.ReadStreamSRTP)
	r.rtcpReadStreams = make(map[string]*srtp.ReadStreamSRTCP)
	r.rtpReadStreamsReady = make(map[string]chan struct{})
	r.rtcpReadStreamsReady = make(map[string]chan struct{})

	r.track = &Track{
		kind:     r.kind,
		ssrc:     parameters.Encodings[0].SSRC,
		receiver: r,
	}

	for _, enc := range parameters.Encodings {
		// use the ssrc (since it's fixed) as the stream index
		streamID := defaultRid //strconv.FormatUint(uint64(enc.SSRC), 10)
		if r.useRid {
			if enc.RID == "" {
				return fmt.Errorf("receiver is rid based but encoding doesn't have a rid")
			}
			streamID = enc.RID
		}

		r.tracks[streamID] = &Track{
			id:   streamID,
			rid:  enc.RID,
			ssrc: enc.SSRC,
		}

		r.rtpReadStreamsReady[streamID] = make(chan struct{})
		r.rtcpReadStreamsReady[streamID] = make(chan struct{})
	}

	if !r.useRid {
		srtpSession, err := r.transport.getSRTPSession()
		if err != nil {
			return err
		}

		r.rtpReadStreams[defaultRid], err = srtpSession.OpenReadStream(parameters.Encodings[0].SSRC)
		if err != nil {
			return err
		}

		srtcpSession, err := r.transport.getSRTCPSession()
		if err != nil {
			return err
		}

		r.rtcpReadStreams[defaultRid], err = srtcpSession.OpenReadStream(parameters.Encodings[0].SSRC)
		if err != nil {
			return err
		}

		close(r.rtpReadStreamsReady[defaultRid])
		close(r.rtcpReadStreamsReady[defaultRid])
	}

	return nil
}

// Read reads incoming RTCP for this RTPReceiver
func (r *RTPReceiver) Read(b []byte) (n int, err error) {
	select {
	case <-r.initialized:
		return r.rtcpReadStream.Read(b)
	case <-r.closed:
		return 0, io.ErrClosedPipe
	}
}

// ReadStreamID reads incoming RTCP for this RTPReceiver
func (r *RTPReceiver) ReadStreamID(b []byte, streamID string) (n int, err error) {
	<-r.rtpReadStreamsReady[streamID]
	return r.rtpReadStreams[streamID].Read(b)
}

// ReadRTCP is a convenience method that wraps Read and unmarshals for you
func (r *RTPReceiver) ReadRTCP() ([]rtcp.Packet, error) {
	b := make([]byte, receiveMTU)
	i, err := r.Read(b)
	if err != nil {
		return nil, err
	}

	return rtcp.Unmarshal(b[:i])
}

// ReadRTCPStreamID is a convenience method that wraps Read and unmarshals for you
func (r *RTPReceiver) ReadRTCPStreamID(streamID string) ([]rtcp.Packet, error) {
	<-r.rtpReadStreamsReady[streamID]

	b := make([]byte, receiveMTU)
	i, err := r.rtcpReadStreams[streamID].Read(b)
	if err != nil {
		return nil, err
	}

	return rtcp.Unmarshal(b[:i])
}

func (r *RTPReceiver) haveReceived() bool {
	select {
	case <-r.initialized:
		return true
	default:
		return false
	}
}

// Stop irreversibly stops the RTPReceiver
func (r *RTPReceiver) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	select {
	case <-r.closed:
		return nil
	default:
	}

	for _, s := range r.rtpReadStreams {
		if s != nil {
			if err := s.Close(); err != nil {
				return err
			}
		}
	}

	for _, s := range r.rtcpReadStreams {
		if s != nil {
			if err := s.Close(); err != nil {
				return err
			}
		}
	}

	close(r.closed)
	return nil
}

// readRTP should only be called by a track, this only exists so we can keep state in one place
func (r *RTPReceiver) readRTP(b []byte) (n int, err error) {
	<-r.initialized
	return r.rtpReadStream.Read(b)
}

func (r *RTPReceiver) readRTPStreamID(b []byte, streamID string) (n int, err error) {
	<-r.rtpReadStreamsReady[streamID]
	return r.rtpReadStreams[streamID].Read(b)
}

// setRTPReadStream sets a rtpReadStream. The stream index is the rid if the receiver is rid based or the ssrc if not rid based
func (r *RTPReceiver) setRTPReadStream(rs *srtp.ReadStreamSRTP, rid string, ssrc uint32, payloadType uint8, codec *RTPCodec) {
	<-r.initialized

	r.mu.Lock()
	defer r.mu.Unlock()

	streamID := strconv.FormatUint(uint64(ssrc), 10)
	if r.useRid {
		streamID = rid
	}

	if r.rtpReadStreams[streamID] != nil {
		return
	}

	r.rtpReadStreams[streamID] = rs

	// open a rtcp read stream for the same ssrc
	srtcpSession, _ := r.transport.getSRTCPSession()
	r.rtcpReadStreams[streamID], _ = srtcpSession.OpenReadStream(ssrc)

	close(r.rtpReadStreamsReady[streamID])
	close(r.rtcpReadStreamsReady[streamID])

	r.track.mu.Lock()
	r.tracks[streamID].mu.Lock()
	r.tracks[streamID].ready = true
	r.tracks[streamID].ssrc = ssrc
	r.tracks[streamID].mu.Unlock()

	// Set the same payload for all streams
	// TODO(sgotti) handle different payloaf for streams in the same track? Currently no implementation have different payloads
	for _, track := range r.tracks {
		track.mu.Lock()
		track.payloadType = payloadType
		track.codec = codec
		track.mu.Unlock()
	}
	r.track.mu.Unlock()
}
