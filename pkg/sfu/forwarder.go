// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sfu

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/codecmunger"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
)

// Forwarder
const (
	FlagPauseOnDowngrade  = true
	FlagFilterRTX         = false
	FlagFilterRTXLayers   = true
	TransitionCostSpatial = 10

	ResumeBehindThresholdSeconds      = float64(0.2)   // 200ms
	ResumeBehindHighTresholdSeconds   = float64(2.0)   // 2 seconds
	LayerSwitchBehindThresholdSeconds = float64(0.05)  // 50ms
	SwitchAheadThresholdSeconds       = float64(0.025) // 25ms
)

// -------------------------------------------------------------------

type VideoPauseReason int

const (
	VideoPauseReasonNone VideoPauseReason = iota
	VideoPauseReasonMuted
	VideoPauseReasonPubMuted
	VideoPauseReasonFeedDry
	VideoPauseReasonBandwidth
)

func (v VideoPauseReason) String() string {
	switch v {
	case VideoPauseReasonNone:
		return "NONE"
	case VideoPauseReasonMuted:
		return "MUTED"
	case VideoPauseReasonPubMuted:
		return "PUB_MUTED"
	case VideoPauseReasonFeedDry:
		return "FEED_DRY"
	case VideoPauseReasonBandwidth:
		return "BANDWIDTH"
	default:
		return fmt.Sprintf("%d", int(v))
	}
}

// -------------------------------------------------------------------

type VideoAllocation struct {
	PauseReason         VideoPauseReason
	IsDeficient         bool
	BandwidthRequested  int64
	BandwidthDelta      int64
	BandwidthNeeded     int64
	Bitrates            Bitrates
	TargetLayer         buffer.VideoLayer
	RequestLayerSpatial int32
	MaxLayer            buffer.VideoLayer
	DistanceToDesired   float64
}

func (v *VideoAllocation) String() string {
	return fmt.Sprintf("VideoAllocation{pause: %s, def: %+v, bwr: %d, del: %d, bwn: %d, rates: %+v, target: %s, req: %d, max: %s, dist: %0.2f}",
		v.PauseReason,
		v.IsDeficient,
		v.BandwidthRequested,
		v.BandwidthDelta,
		v.BandwidthNeeded,
		v.Bitrates,
		v.TargetLayer,
		v.RequestLayerSpatial,
		v.MaxLayer,
		v.DistanceToDesired,
	)
}

func (v *VideoAllocation) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if v == nil {
		return nil
	}

	e.AddString("PauseReason", v.PauseReason.String())
	e.AddBool("IsDeficient", v.IsDeficient)
	e.AddInt64("BandwidthRquested", v.BandwidthRequested)
	e.AddInt64("BandwidthDelta", v.BandwidthDelta)
	e.AddInt64("BandwidthNeeded", v.BandwidthNeeded)
	e.AddReflected("Bitrates", v.Bitrates)
	e.AddReflected("TargetLayer", v.TargetLayer)
	e.AddInt32("RequestLayerSpatial", v.RequestLayerSpatial)
	e.AddReflected("MaxLayer", v.MaxLayer)
	e.AddFloat64("DistanceToDesired", v.DistanceToDesired)
	return nil
}

var (
	VideoAllocationDefault = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry, // start with no feed till feed is seen
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.InvalidLayer,
	}
)

// -------------------------------------------------------------------

type VideoAllocationProvisional struct {
	muted           bool
	pubMuted        bool
	maxSeenLayer    buffer.VideoLayer
	availableLayers []int32
	bitrates        Bitrates
	maxLayer        buffer.VideoLayer
	currentLayer    buffer.VideoLayer
	allocatedLayer  buffer.VideoLayer
}

// -------------------------------------------------------------------

type VideoTransition struct {
	From           buffer.VideoLayer
	To             buffer.VideoLayer
	BandwidthDelta int64
}

func (v *VideoTransition) String() string {
	return fmt.Sprintf("VideoTransition{from: %s, to: %s, del: %d}", v.From, v.To, v.BandwidthDelta)
}

func (v *VideoTransition) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if v == nil {
		return nil
	}

	e.AddReflected("From", v.From)
	e.AddReflected("To", v.To)
	e.AddInt64("BandwidthDelta", v.BandwidthDelta)
	return nil
}

// -------------------------------------------------------------------

type TranslationParams struct {
	shouldDrop  bool
	isResuming  bool
	isSwitching bool
	rtp         TranslationParamsRTP
	ddBytes     []byte
	marker      bool
}

// -------------------------------------------------------------------

type ForwarderState struct {
	Started               bool
	ReferenceLayerSpatial int32
	PreStartTime          time.Time
	ExtFirstTS            uint64
	RefTSOffset           uint64
	RTP                   RTPMungerState
	Codec                 interface{}
}

func (f ForwarderState) String() string {
	codecString := ""
	switch codecState := f.Codec.(type) {
	case codecmunger.VP8State:
		codecString = codecState.String()
	}
	return fmt.Sprintf("ForwarderState{started: %v, referenceLayerSpatial: %d, preStartTime: %s, extFirstTS: %d, refTSOffset: %d, rtp: %s, codec: %s}",
		f.Started,
		f.ReferenceLayerSpatial,
		f.PreStartTime.String(),
		f.ExtFirstTS,
		f.RefTSOffset,
		f.RTP.String(),
		codecString,
	)
}

// -------------------------------------------------------------------

type Forwarder struct {
	lock                          sync.RWMutex
	codec                         webrtc.RTPCodecCapability
	kind                          webrtc.RTPCodecType
	logger                        logger.Logger
	getReferenceLayerRTPTimestamp func(ts uint32, layer int32, referenceLayer int32) (uint32, error)
	getExpectedRTPTimestamp       func(at time.Time) (uint64, error)

	muted                 bool
	pubMuted              bool
	resumeBehindThreshold float64

	started               bool
	preStartTime          time.Time
	extFirstTS            uint64
	lastSSRC              uint32
	referenceLayerSpatial int32
	refTSOffset           uint64

	provisional *VideoAllocationProvisional

	lastAllocation VideoAllocation

	rtpMunger *RTPMunger

	vls videolayerselector.VideoLayerSelector

	codecMunger codecmunger.CodecMunger
}

func NewForwarder(
	kind webrtc.RTPCodecType,
	logger logger.Logger,
	getReferenceLayerRTPTimestamp func(ts uint32, layer int32, referenceLayer int32) (uint32, error),
	getExpectedRTPTimestamp func(at time.Time) (uint64, error),
) *Forwarder {
	f := &Forwarder{
		kind:                          kind,
		logger:                        logger,
		getReferenceLayerRTPTimestamp: getReferenceLayerRTPTimestamp,
		getExpectedRTPTimestamp:       getExpectedRTPTimestamp,
		referenceLayerSpatial:         buffer.InvalidLayerSpatial,
		lastAllocation:                VideoAllocationDefault,
		rtpMunger:                     NewRTPMunger(logger),
		vls:                           videolayerselector.NewNull(logger),
		codecMunger:                   codecmunger.NewNull(logger),
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.vls.SetMaxTemporal(buffer.DefaultMaxLayerTemporal)
	}
	return f
}

func (f *Forwarder) SetMaxPublishedLayer(maxPublishedLayer int32) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	existingMaxSeen := f.vls.GetMaxSeen()
	if maxPublishedLayer <= existingMaxSeen.Spatial {
		return false
	}

	f.vls.SetMaxSeenSpatial(maxPublishedLayer)
	f.logger.Debugw("setting max published layer", "layer", maxPublishedLayer)
	return true
}

func (f *Forwarder) SetMaxTemporalLayerSeen(maxTemporalLayerSeen int32) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	existingMaxSeen := f.vls.GetMaxSeen()
	if maxTemporalLayerSeen <= existingMaxSeen.Temporal {
		return false
	}

	f.vls.SetMaxSeenTemporal(maxTemporalLayerSeen)
	f.logger.Debugw("setting max temporal layer seen", "maxTemporalLayerSeen", maxTemporalLayerSeen)
	return true
}

func (f *Forwarder) DetermineCodec(codec webrtc.RTPCodecCapability, extensions []webrtc.RTPHeaderExtensionParameter) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.codec.MimeType != "" {
		return
	}
	f.codec = codec

	ddAvailable := func(exts []webrtc.RTPHeaderExtensionParameter) bool {
		for _, ext := range exts {
			if ext.URI == dd.ExtensionURI {
				return true
			}
		}
		return false
	}

	switch strings.ToLower(codec.MimeType) {
	case "video/vp8":
		f.codecMunger = codecmunger.NewVP8FromNull(f.codecMunger, f.logger)
		if f.vls != nil {
			f.vls = videolayerselector.NewSimulcastFromNull(f.vls)
		} else {
			f.vls = videolayerselector.NewSimulcast(f.logger)
		}
		f.vls.SetTemporalLayerSelector(temporallayerselector.NewVP8(f.logger))
	case "video/h264":
		if f.vls != nil {
			f.vls = videolayerselector.NewSimulcastFromNull(f.vls)
		} else {
			f.vls = videolayerselector.NewSimulcast(f.logger)
		}
	case "video/vp9":
		isDDAvailable := ddAvailable(extensions)

		if isDDAvailable {
			if f.vls != nil {
				f.vls = videolayerselector.NewDependencyDescriptorFromNull(f.vls)
			} else {
				f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
			}
		} else {
			if f.vls != nil {
				f.vls = videolayerselector.NewVP9FromNull(f.vls)
			} else {
				f.vls = videolayerselector.NewVP9(f.logger)
			}
		}
		// SVC-TODO: Support for VP9 simulcast. When DD is not available, have to pick selector based on VP9 SVC or Simulcast
	case "video/av1":
		// DD-TODO : we only enable dd layer selector for av1/vp9 now, in the future we can enable it for vp8 too

		isDDAvailable := ddAvailable(extensions)
		if isDDAvailable {
			if f.vls != nil {
				f.vls = videolayerselector.NewDependencyDescriptorFromNull(f.vls)
			} else {
				f.vls = videolayerselector.NewDependencyDescriptor(f.logger)
			}
		} else {
			if f.vls != nil {
				f.vls = videolayerselector.NewSimulcastFromNull(f.vls)
			} else {
				f.vls = videolayerselector.NewSimulcast(f.logger)
			}
		}
		// SVC-TODO: Support for AV1 Simulcast
	}
}

func (f *Forwarder) GetState() ForwarderState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if !f.started {
		return ForwarderState{}
	}

	return ForwarderState{
		Started:               f.started,
		ReferenceLayerSpatial: f.referenceLayerSpatial,
		PreStartTime:          f.preStartTime,
		ExtFirstTS:            f.extFirstTS,
		RefTSOffset:           f.refTSOffset,
		RTP:                   f.rtpMunger.GetLast(),
		Codec:                 f.codecMunger.GetState(),
	}
}

func (f *Forwarder) SeedState(state ForwarderState) {
	if !state.Started {
		return
	}

	f.lock.Lock()
	defer f.lock.Unlock()

	f.rtpMunger.SeedLast(state.RTP)
	f.codecMunger.SeedState(state.Codec)

	f.started = true
	f.referenceLayerSpatial = state.ReferenceLayerSpatial
	f.preStartTime = state.PreStartTime
	f.extFirstTS = state.ExtFirstTS
	f.refTSOffset = state.RefTSOffset
}

func (f *Forwarder) Mute(muted bool, isSubscribeMutable bool) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted == muted {
		return false
	}

	// Do not mute when paused due to bandwidth limitation.
	// There are two issues
	//   1. Muting means probing cannot happen on this track.
	//   2. Muting also triggers notification to publisher about layers this forwarder needs.
	//      If this forwarder does not need any layer, publisher could turn off all layers.
	// So, muting could lead to not being able to restart the track.
	// To avoid that, ignore mute when paused due to bandwidth limitations.
	//
	// NOTE: The above scenario refers to mute getting triggered due
	// to video stream visibility changes. When a stream is paused, it is possible
	// that the receiver hides the video tile triggering subscription mute.
	// The work around here to ignore mute does ignore an intentional mute.
	// It could result in some bandwidth consumed for stream without visibility in
	// the case of intentional mute.
	if muted && !isSubscribeMutable {
		f.logger.Debugw("ignoring forwarder mute, paused due to congestion")
		return false
	}

	f.logger.Debugw("setting forwarder mute", "muted", muted)
	f.muted = muted

	// resync when muted so that sequence numbers do not jump on unmute
	if muted {
		f.resyncLocked()
	}

	return true
}

func (f *Forwarder) IsMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted
}

func (f *Forwarder) PubMute(pubMuted bool) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.pubMuted == pubMuted {
		return false
	}

	f.logger.Debugw("setting forwarder pub mute", "muted", pubMuted)
	f.pubMuted = pubMuted

	// resync when pub muted so that sequence numbers do not jump on unmute
	if pubMuted {
		f.resyncLocked()
	}
	return true
}

func (f *Forwarder) IsPubMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.pubMuted
}

func (f *Forwarder) IsAnyMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted || f.pubMuted
}

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false, buffer.InvalidLayer
	}

	existingMax := f.vls.GetMax()
	if spatialLayer == existingMax.Spatial {
		return false, existingMax
	}

	f.logger.Debugw("setting max spatial layer", "layer", spatialLayer)
	f.vls.SetMaxSpatial(spatialLayer)
	return true, f.vls.GetMax()
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return false, buffer.InvalidLayer
	}

	existingMax := f.vls.GetMax()
	if temporalLayer == existingMax.Temporal {
		return false, existingMax
	}

	f.logger.Debugw("setting max temporal layer", "layer", temporalLayer)
	f.vls.SetMaxTemporal(temporalLayer)
	return true, f.vls.GetMax()
}

func (f *Forwarder) MaxLayer() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.GetMax()
}

func (f *Forwarder) CurrentLayer() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.GetCurrent()
}

func (f *Forwarder) TargetLayer() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.GetTarget()
}

func (f *Forwarder) GetMaxSubscribedSpatial() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	layer := buffer.InvalidLayerSpatial // covers muted case
	if !f.muted {
		layer = f.vls.GetMax().Spatial

		// If current is higher, mark the current layer as max subscribed layer
		// to prevent the current layer from stopping before forwarder switches
		// to the new and lower max layer,
		if layer < f.vls.GetCurrent().Spatial {
			layer = f.vls.GetCurrent().Spatial
		}
	}

	return layer
}

func (f *Forwarder) GetCurrentSpatialAndTSOffset() (int32, uint64) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return 0, f.rtpMunger.GetPinnedTSOffset()
	}

	return f.vls.GetCurrent().Spatial, f.rtpMunger.GetPinnedTSOffset()
}

func (f *Forwarder) isDeficientLocked() bool {
	return f.lastAllocation.IsDeficient
}

func (f *Forwarder) IsDeficient() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.isDeficientLocked()
}

func (f *Forwarder) PauseReason() VideoPauseReason {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.PauseReason
}

func (f *Forwarder) BandwidthRequested(brs Bitrates) int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getBandwidthNeeded(brs, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
}

func (f *Forwarder) DistanceToDesired(availableLayers []int32, brs Bitrates) float64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getDistanceToDesired(
		f.muted,
		f.pubMuted,
		f.vls.GetMaxSeen(),
		availableLayers,
		brs,
		f.vls.GetTarget(),
		f.vls.GetMax(),
	)
}

func (f *Forwarder) GetOptimalBandwidthNeeded(brs Bitrates) int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getOptimalBandwidthNeeded(f.muted, f.pubMuted, f.vls.GetMaxSeen().Spatial, brs, f.vls.GetMax())
}

func (f *Forwarder) AllocateOptimal(availableLayers []int32, brs Bitrates, allowOvershoot bool) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation
	}

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	currentLayer := f.vls.GetCurrent()
	requestSpatial := f.vls.GetRequestSpatial()
	alloc := VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		Bitrates:            brs,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: requestSpatial,
		MaxLayer:            maxLayer,
	}
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)
	if optimalBandwidthNeeded == 0 {
		alloc.PauseReason = VideoPauseReasonFeedDry
	}
	alloc.BandwidthNeeded = optimalBandwidthNeeded

	getMaxTemporal := func() int32 {
		maxTemporal := maxLayer.Temporal
		if maxSeenLayer.Temporal != buffer.InvalidLayerTemporal && maxSeenLayer.Temporal < maxTemporal {
			maxTemporal = maxSeenLayer.Temporal
		}
		return maxTemporal
	}

	opportunisticAlloc := func() {
		// opportunistically latch on to anything
		maxSpatial := maxLayer.Spatial
		if allowOvershoot && f.vls.IsOvershootOkay() && maxSeenLayer.Spatial > maxSpatial {
			maxSpatial = maxSeenLayer.Spatial
		}

		alloc.TargetLayer = buffer.VideoLayer{
			Spatial:  int32(math.Min(float64(maxSeenLayer.Spatial), float64(maxSpatial))),
			Temporal: getMaxTemporal(),
		}
	}

	switch {
	case !maxLayer.IsValid() || maxSeenLayer.Spatial == buffer.InvalidLayerSpatial:
		// nothing to do when max layers are not valid OR max published layer is invalid

	case f.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	default:
		// lots of different events could end up here
		//   1. Publisher side layer resuming/stopping
		//   2. Bitrate becoming available
		//   3. New max published spatial layer or max temporal layer seen
		//   4. Subscriber layer changes
		//
		// to handle all of the above
		//   1. Find highest that can be requested - takes into account available layers and overshoot.
		//      This should catch scenarios like layers resuming/stopping.
		//   2. If current is a valid layer, check against currently available layers and continue at current
		//      if possible. Else, choose the highest available layer as the next target.
		//   3. If current is not valid, set next target to be opportunistic.
		maxLayerSpatialLimit := int32(math.Min(float64(maxLayer.Spatial), float64(maxSeenLayer.Spatial)))
		highestAvailableLayer := buffer.InvalidLayerSpatial
		requestLayerSpatial := buffer.InvalidLayerSpatial
		for _, al := range availableLayers {
			if al > requestLayerSpatial && al <= maxLayerSpatialLimit {
				requestLayerSpatial = al
			}
			if al > highestAvailableLayer {
				highestAvailableLayer = al
			}
		}
		if requestLayerSpatial == buffer.InvalidLayerSpatial && highestAvailableLayer != buffer.InvalidLayerSpatial && allowOvershoot && f.vls.IsOvershootOkay() {
			requestLayerSpatial = highestAvailableLayer
		}

		if currentLayer.IsValid() {
			if (requestLayerSpatial == requestSpatial && currentLayer.Spatial == requestSpatial) || requestLayerSpatial == buffer.InvalidLayerSpatial {
				// 1. current is locked to desired, stay there
				// OR
				// 2. feed may be dry, let it continue at current layer if valid.
				// covers the cases of
				//   1. mis-detection of layer stop - can continue streaming
				//   2. current layer resuming - can latch on when it starts
				alloc.TargetLayer = buffer.VideoLayer{
					Spatial:  currentLayer.Spatial,
					Temporal: getMaxTemporal(),
				}
			} else {
				// current layer has stopped, switch to highest available
				alloc.TargetLayer = buffer.VideoLayer{
					Spatial:  requestLayerSpatial,
					Temporal: getMaxTemporal(),
				}
			}
			alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial
		} else {
			// opportunistically latch on to anything
			opportunisticAlloc()
			if requestLayerSpatial == buffer.InvalidLayerSpatial {
				alloc.RequestLayerSpatial = maxLayerSpatialLimit
			} else {
				alloc.RequestLayerSpatial = requestLayerSpatial
			}
		}
	}

	if !alloc.TargetLayer.IsValid() {
		alloc.TargetLayer = buffer.InvalidLayer
		alloc.RequestLayerSpatial = buffer.InvalidLayerSpatial
	}
	if alloc.TargetLayer.IsValid() {
		alloc.BandwidthRequested = optimalBandwidthNeeded
	}
	alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(brs, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
	alloc.DistanceToDesired = getDistanceToDesired(
		f.muted,
		f.pubMuted,
		f.vls.GetMaxSeen(),
		availableLayers,
		brs,
		alloc.TargetLayer,
		f.vls.GetMax(),
	)

	return f.updateAllocation(alloc, "optimal")
}

func (f *Forwarder) ProvisionalAllocatePrepare(availableLayers []int32, bitrates Bitrates) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional = &VideoAllocationProvisional{
		allocatedLayer: buffer.InvalidLayer,
		muted:          f.muted,
		pubMuted:       f.pubMuted,
		maxSeenLayer:   f.vls.GetMaxSeen(),
		bitrates:       bitrates,
		maxLayer:       f.vls.GetMax(),
		currentLayer:   f.vls.GetCurrent(),
	}

	f.provisional.availableLayers = make([]int32, len(availableLayers))
	copy(f.provisional.availableLayers, availableLayers)
}

func (f *Forwarder) ProvisionalAllocateReset() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional.allocatedLayer = buffer.InvalidLayer
}

func (f *Forwarder) ProvisionalAllocate(availableChannelCapacity int64, layer buffer.VideoLayer, allowPause bool, allowOvershoot bool) (bool, int64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted ||
		f.provisional.pubMuted ||
		f.provisional.maxSeenLayer.Spatial == buffer.InvalidLayerSpatial ||
		!f.provisional.maxLayer.IsValid() ||
		((!allowOvershoot || !f.vls.IsOvershootOkay()) && layer.GreaterThan(f.provisional.maxLayer)) {
		return false, 0
	}

	requiredBitrate := f.provisional.bitrates[layer.Spatial][layer.Temporal]
	if requiredBitrate == 0 {
		return false, 0
	}

	alreadyAllocatedBitrate := int64(0)
	if f.provisional.allocatedLayer.IsValid() {
		alreadyAllocatedBitrate = f.provisional.bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
	}

	// a layer under maximum fits, take it
	if !layer.GreaterThan(f.provisional.maxLayer) && requiredBitrate <= (availableChannelCapacity+alreadyAllocatedBitrate) {
		f.provisional.allocatedLayer = layer
		return true, requiredBitrate - alreadyAllocatedBitrate
	}

	//
	// Given layer does not fit.
	//
	// Could be one of
	//  1. a layer below maximum that does not fit
	//  2. a layer above maximum which may or may not fit, but overshoot is allowed.
	// In any of those cases, take the lowest possible layer if pause is not allowed
	//
	if !allowPause && (!f.provisional.allocatedLayer.IsValid() || !layer.GreaterThan(f.provisional.allocatedLayer)) {
		f.provisional.allocatedLayer = layer
		return true, requiredBitrate - alreadyAllocatedBitrate
	}

	return false, 0
}

func (f *Forwarder) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) (VideoTransition, []int32, Bitrates) {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested.
	//
	// The goal is to provide a co-operative transition. Co-operative stream allocation aims to keep all the streams active
	// as much as possible.
	//
	// When channel is congested, effecting a transition which will consume more bits will lead to more congestion.
	// So, this routine does the following
	//   1. When muting, it is not going to increase consumption.
	//   2. If the stream is currently active and the transition needs more bits (higher layers = more bits), do not make the up move.
	//      The higher layer requirement could be due to a new published layer becoming available or subscribed layers changing.
	//   3. If the new target layers are lower than current target, take the move down and save bits.
	//   4. If not currently streaming, find the minimum layers that can unpause the stream.
	//
	// To summarize, co-operative streaming means
	//   - Try to keep tracks streaming, i.e. no pauses at the expense of some streams not being at optimal layers
	//   - Do not make an upgrade as it could affect other tracks
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	existingTargetLayer := f.vls.GetTarget()
	if f.provisional.muted || f.provisional.pubMuted {
		f.provisional.allocatedLayer = buffer.InvalidLayer
		return VideoTransition{
			From:           existingTargetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: -getBandwidthNeeded(f.provisional.bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
		}, f.provisional.availableLayers, f.provisional.bitrates
	}

	// check if we should preserve current target
	if existingTargetLayer.IsValid() {
		// what is the highest that is available
		maximalLayer := buffer.InvalidLayer
		maximalBandwidthRequired := int64(0)
		for s := f.provisional.maxLayer.Spatial; s >= 0; s-- {
			for t := f.provisional.maxLayer.Temporal; t >= 0; t-- {
				if f.provisional.bitrates[s][t] != 0 {
					maximalLayer = buffer.VideoLayer{Spatial: s, Temporal: t}
					maximalBandwidthRequired = f.provisional.bitrates[s][t]
					break
				}
			}

			if maximalBandwidthRequired != 0 {
				break
			}
		}

		if maximalLayer.IsValid() {
			if !existingTargetLayer.GreaterThan(maximalLayer) && f.provisional.bitrates[existingTargetLayer.Spatial][existingTargetLayer.Temporal] != 0 {
				// currently streaming and maybe wanting an upgrade (existingTargetLayer <= maximalLayer),
				// just preserve current target in the cooperative scheme of things
				f.provisional.allocatedLayer = existingTargetLayer
				return VideoTransition{
					From:           existingTargetLayer,
					To:             existingTargetLayer,
					BandwidthDelta: 0,
				}, f.provisional.availableLayers, f.provisional.bitrates
			}

			if existingTargetLayer.GreaterThan(maximalLayer) {
				// maximalLayer < existingTargetLayer, make the down move
				f.provisional.allocatedLayer = maximalLayer
				return VideoTransition{
					From:           existingTargetLayer,
					To:             maximalLayer,
					BandwidthDelta: maximalBandwidthRequired - getBandwidthNeeded(f.provisional.bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
				}, f.provisional.availableLayers, f.provisional.bitrates
			}
		}
	}

	findNextLayer := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (buffer.VideoLayer, int64) {
		layers := buffer.InvalidLayer
		bw := int64(0)
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				if f.provisional.bitrates[s][t] != 0 {
					layers = buffer.VideoLayer{Spatial: s, Temporal: t}
					bw = f.provisional.bitrates[s][t]
					break
				}
			}

			if bw != 0 {
				break
			}
		}

		return layers, bw
	}

	targetLayer := buffer.InvalidLayer
	bandwidthRequired := int64(0)
	if !existingTargetLayer.IsValid() {
		// currently not streaming, find minimal
		// NOTE: a layer in feed could have paused and there could be other options than going back to minimal,
		// but the cooperative scheme knocks things back to minimal
		targetLayer, bandwidthRequired = findNextLayer(
			0, f.provisional.maxLayer.Spatial,
			0, f.provisional.maxLayer.Temporal,
		)

		// could not find a minimal layer, overshoot if allowed
		if bandwidthRequired == 0 && f.provisional.maxLayer.IsValid() && allowOvershoot && f.vls.IsOvershootOkay() {
			targetLayer, bandwidthRequired = findNextLayer(
				f.provisional.maxLayer.Spatial+1, buffer.DefaultMaxLayerSpatial,
				0, buffer.DefaultMaxLayerTemporal,
			)
		}
	}

	// if nothing available, just leave target at current to enable opportunistic forwarding in case current resumes
	if !targetLayer.IsValid() {
		targetLayer = f.provisional.currentLayer
		if targetLayer.IsValid() {
			bandwidthRequired = f.provisional.bitrates[targetLayer.Spatial][targetLayer.Temporal]
		}
	}

	f.provisional.allocatedLayer = targetLayer
	return VideoTransition{
		From:           f.vls.GetTarget(),
		To:             targetLayer,
		BandwidthDelta: bandwidthRequired - getBandwidthNeeded(f.provisional.bitrates, existingTargetLayer, f.lastAllocation.BandwidthRequested),
	}, f.provisional.availableLayers, f.provisional.bitrates
}

func (f *Forwarder) ProvisionalAllocateGetBestWeightedTransition() (VideoTransition, []int32, Bitrates) {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested. This is called on tracks other than the one needing the change. When the track
	// needing the change requires bits, this is called to check if this track can contribute some bits to the pool.
	//
	// The goal is to keep all tracks streaming as much as possible. So, the track that needs a change needs bandwidth to be unpaused.
	//
	// This tries to figure out how much this track can contribute back to the pool to enable the track that needs to be unpaused.
	//   1. Track muted OR feed dry - can contribute everything back in case it was using bandwidth.
	//   2. Look at all possible down transitions from current target and find the best offer.
	//      Best offer is calculated as bandwidth saved moving to a down layer divided by cost.
	//      Cost has two components
	//        a. Transition cost: Spatial layer switch is expensive due to key frame requirement, but temporal layer switch is free.
	//        b. Quality cost: The farther away from desired layers, the higher the quality cost.
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	targetLayer := f.vls.GetTarget()
	if f.provisional.muted || f.provisional.pubMuted {
		f.provisional.allocatedLayer = buffer.InvalidLayer
		return VideoTransition{
			From:           targetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: 0 - getBandwidthNeeded(f.provisional.bitrates, targetLayer, f.lastAllocation.BandwidthRequested),
		}, f.provisional.availableLayers, f.provisional.bitrates
	}

	maxReachableLayerTemporal := buffer.InvalidLayerTemporal
	for t := f.provisional.maxLayer.Temporal; t >= 0; t-- {
		for s := f.provisional.maxLayer.Spatial; s >= 0; s-- {
			if f.provisional.bitrates[s][t] != 0 {
				maxReachableLayerTemporal = t
				break
			}
		}
		if maxReachableLayerTemporal != buffer.InvalidLayerTemporal {
			break
		}
	}

	if maxReachableLayerTemporal == buffer.InvalidLayerTemporal {
		// feed has gone dry, just leave target at current to enable opportunistic forwarding in case current resumes.
		// Note that this is giving back bits and opportunistic forwarding resuming might trigger congestion again,
		// but that should be handled by stream allocator.
		f.provisional.allocatedLayer = f.provisional.currentLayer
		return VideoTransition{
			From:           targetLayer,
			To:             f.provisional.allocatedLayer,
			BandwidthDelta: 0 - getBandwidthNeeded(f.provisional.bitrates, targetLayer, f.lastAllocation.BandwidthRequested),
		}, f.provisional.availableLayers, f.provisional.bitrates
	}

	// starting from minimum to target, find transition which gives the best
	// transition taking into account bits saved vs cost of such a transition
	existingBandwidthNeeded := getBandwidthNeeded(f.provisional.bitrates, targetLayer, f.lastAllocation.BandwidthRequested)
	bestLayer := buffer.InvalidLayer
	bestBandwidthDelta := int64(0)
	bestValue := float32(0)
	for s := int32(0); s <= targetLayer.Spatial; s++ {
		for t := int32(0); t <= targetLayer.Temporal; t++ {
			if s == targetLayer.Spatial && t == targetLayer.Temporal {
				break
			}

			bandwidthDelta := int64(math.Max(float64(0), float64(existingBandwidthNeeded-f.provisional.bitrates[s][t])))

			transitionCost := int32(0)
			// SVC-TODO: SVC will need a different cost transition
			if targetLayer.Spatial != s {
				transitionCost = TransitionCostSpatial
			}

			qualityCost := (maxReachableLayerTemporal+1)*(targetLayer.Spatial-s) + (targetLayer.Temporal - t)

			value := float32(0)
			if (transitionCost + qualityCost) != 0 {
				value = float32(bandwidthDelta) / float32(transitionCost+qualityCost)
			}
			if value > bestValue || (value == bestValue && bandwidthDelta > bestBandwidthDelta) {
				bestValue = value
				bestBandwidthDelta = bandwidthDelta
				bestLayer = buffer.VideoLayer{Spatial: s, Temporal: t}
			}
		}
	}

	f.provisional.allocatedLayer = bestLayer
	return VideoTransition{
		From:           targetLayer,
		To:             bestLayer,
		BandwidthDelta: -bestBandwidthDelta,
	}, f.provisional.availableLayers, f.provisional.bitrates
}

func (f *Forwarder) ProvisionalAllocateCommit() VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := getOptimalBandwidthNeeded(
		f.provisional.muted,
		f.provisional.pubMuted,
		f.provisional.maxSeenLayer.Spatial,
		f.provisional.bitrates,
		f.provisional.maxLayer,
	)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0 - getBandwidthNeeded(f.provisional.bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested),
		Bitrates:            f.provisional.bitrates,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayer:         f.provisional.allocatedLayer,
		RequestLayerSpatial: f.provisional.allocatedLayer.Spatial,
		MaxLayer:            f.provisional.maxLayer,
		DistanceToDesired: getDistanceToDesired(
			f.provisional.muted,
			f.provisional.pubMuted,
			f.provisional.maxSeenLayer,
			f.provisional.availableLayers,
			f.provisional.bitrates,
			f.provisional.allocatedLayer,
			f.provisional.maxLayer,
		),
	}

	switch {
	case f.provisional.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.provisional.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	case optimalBandwidthNeeded == 0:
		if f.provisional.allocatedLayer.IsValid() {
			// overshoot
			alloc.BandwidthRequested = f.provisional.bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
			alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(f.provisional.bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)
		} else {
			alloc.PauseReason = VideoPauseReasonFeedDry

			// leave target at current for opportunistic forwarding
			if f.provisional.currentLayer.IsValid() && f.provisional.currentLayer.Spatial <= f.provisional.maxLayer.Spatial {
				f.provisional.allocatedLayer = f.provisional.currentLayer
				alloc.TargetLayer = f.provisional.allocatedLayer
				alloc.RequestLayerSpatial = alloc.TargetLayer.Spatial
			}
		}

	default:
		if f.provisional.allocatedLayer.IsValid() {
			alloc.BandwidthRequested = f.provisional.bitrates[f.provisional.allocatedLayer.Spatial][f.provisional.allocatedLayer.Temporal]
		}
		alloc.BandwidthDelta = alloc.BandwidthRequested - getBandwidthNeeded(f.provisional.bitrates, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested)

		if f.provisional.allocatedLayer.GreaterThan(f.provisional.maxLayer) ||
			alloc.BandwidthRequested >= getOptimalBandwidthNeeded(
				f.provisional.muted,
				f.provisional.pubMuted,
				f.provisional.maxSeenLayer.Spatial,
				f.provisional.bitrates,
				f.provisional.maxLayer,
			) {
			// could be greater than optimal if overshooting
			alloc.IsDeficient = false
		} else {
			alloc.IsDeficient = true
			if !f.provisional.allocatedLayer.IsValid() {
				alloc.PauseReason = VideoPauseReasonBandwidth
			}
		}
	}

	return f.updateAllocation(alloc, "cooperative")
}

func (f *Forwarder) AllocateNextHigher(availableChannelCapacity int64, availableLayers []int32, brs Bitrates, allowOvershoot bool) (VideoAllocation, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation, false
	}

	// if not deficient, nothing to do
	if !f.isDeficientLocked() {
		return f.lastAllocation, false
	}

	// if targets are still pending, don't increase
	targetLayer := f.vls.GetTarget()
	if targetLayer.IsValid() && targetLayer != f.vls.GetCurrent() {
		return f.lastAllocation, false
	}

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)

	alreadyAllocated := int64(0)
	if targetLayer.IsValid() {
		alreadyAllocated = brs[targetLayer.Spatial][targetLayer.Temporal]
	}

	doAllocation := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (bool, VideoAllocation, bool) {
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				bandwidthRequested := brs[s][t]
				if bandwidthRequested == 0 {
					continue
				}

				if (!allowOvershoot || !f.vls.IsOvershootOkay()) && bandwidthRequested-alreadyAllocated > availableChannelCapacity {
					// next higher available layer does not fit, return
					return true, f.lastAllocation, false
				}

				newTargetLayer := buffer.VideoLayer{Spatial: s, Temporal: t}
				alloc := VideoAllocation{
					IsDeficient:         true,
					BandwidthRequested:  bandwidthRequested,
					BandwidthDelta:      bandwidthRequested - alreadyAllocated,
					BandwidthNeeded:     optimalBandwidthNeeded,
					Bitrates:            brs,
					TargetLayer:         newTargetLayer,
					RequestLayerSpatial: newTargetLayer.Spatial,
					MaxLayer:            maxLayer,
					DistanceToDesired: getDistanceToDesired(
						f.muted,
						f.pubMuted,
						maxSeenLayer,
						availableLayers,
						brs,
						newTargetLayer,
						maxLayer,
					),
				}
				if newTargetLayer.GreaterThan(maxLayer) || bandwidthRequested >= optimalBandwidthNeeded {
					alloc.IsDeficient = false
				}

				return true, f.updateAllocation(alloc, "next-higher"), true
			}
		}

		return false, VideoAllocation{}, false
	}

	done := false
	var allocation VideoAllocation
	boosted := false

	// try moving temporal layer up in currently streaming spatial layer
	if targetLayer.IsValid() {
		done, allocation, boosted = doAllocation(
			targetLayer.Spatial, targetLayer.Spatial,
			targetLayer.Temporal+1, maxLayer.Temporal,
		)
		if done {
			return allocation, boosted
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, allocation, boosted = doAllocation(
		targetLayer.Spatial+1, maxLayer.Spatial,
		0, maxLayer.Temporal,
	)
	if done {
		return allocation, boosted
	}

	if allowOvershoot && f.vls.IsOvershootOkay() && maxLayer.IsValid() {
		done, allocation, boosted = doAllocation(
			maxLayer.Spatial+1, buffer.DefaultMaxLayerSpatial,
			0, buffer.DefaultMaxLayerTemporal,
		)
		if done {
			return allocation, boosted
		}
	}

	return f.lastAllocation, false
}

func (f *Forwarder) GetNextHigherTransition(brs Bitrates, allowOvershoot bool) (VideoTransition, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return VideoTransition{}, false
	}

	// if not deficient, nothing to do
	if !f.isDeficientLocked() {
		return VideoTransition{}, false
	}

	// if targets are still pending, don't increase
	targetLayer := f.vls.GetTarget()
	if targetLayer.IsValid() && targetLayer != f.vls.GetCurrent() {
		return VideoTransition{}, false
	}

	alreadyAllocated := int64(0)
	if targetLayer.IsValid() {
		alreadyAllocated = brs[targetLayer.Spatial][targetLayer.Temporal]
	}

	findNextHigher := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (bool, VideoTransition, bool) {
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				bandwidthRequested := brs[s][t]
				// traverse till finding a layer requiring more bits.
				// NOTE: it possible that higher temporal layer of lower spatial layer
				//       could use more bits than lower temporal layer of higher spatial layer.
				if bandwidthRequested == 0 || bandwidthRequested < alreadyAllocated {
					continue
				}

				transition := VideoTransition{
					From:           targetLayer,
					To:             buffer.VideoLayer{Spatial: s, Temporal: t},
					BandwidthDelta: bandwidthRequested - alreadyAllocated,
				}

				return true, transition, true
			}
		}

		return false, VideoTransition{}, false
	}

	done := false
	var transition VideoTransition
	isAvailable := false

	// try moving temporal layer up in currently streaming spatial layer
	maxLayer := f.vls.GetMax()
	if targetLayer.IsValid() {
		done, transition, isAvailable = findNextHigher(
			targetLayer.Spatial, targetLayer.Spatial,
			targetLayer.Temporal+1, maxLayer.Temporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, transition, isAvailable = findNextHigher(
		targetLayer.Spatial+1, maxLayer.Spatial,
		0, maxLayer.Temporal,
	)
	if done {
		return transition, isAvailable
	}

	if allowOvershoot && f.vls.IsOvershootOkay() && maxLayer.IsValid() {
		done, transition, isAvailable = findNextHigher(
			maxLayer.Spatial+1, buffer.DefaultMaxLayerSpatial,
			0, buffer.DefaultMaxLayerTemporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	return VideoTransition{}, false
}

func (f *Forwarder) Pause(availableLayers []int32, brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	maxLayer := f.vls.GetMax()
	maxSeenLayer := f.vls.GetMaxSeen()
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, maxSeenLayer.Spatial, brs, maxLayer)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0 - getBandwidthNeeded(brs, f.vls.GetTarget(), f.lastAllocation.BandwidthRequested),
		Bitrates:            brs,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            maxLayer,
		DistanceToDesired: getDistanceToDesired(
			f.muted,
			f.pubMuted,
			maxSeenLayer,
			availableLayers,
			brs,
			buffer.InvalidLayer,
			maxLayer,
		),
	}

	switch {
	case f.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	case optimalBandwidthNeeded == 0:
		alloc.PauseReason = VideoPauseReasonFeedDry

	default:
		// pausing due to lack of bandwidth
		alloc.IsDeficient = true
		alloc.PauseReason = VideoPauseReasonBandwidth
	}

	return f.updateAllocation(alloc, "pause")
}

func (f *Forwarder) updateAllocation(alloc VideoAllocation, reason string) VideoAllocation {
	// restrict target temporal to 0 if codec does not support temporal layers
	if alloc.TargetLayer.IsValid() && strings.ToLower(f.codec.MimeType) == "video/h264" {
		alloc.TargetLayer.Temporal = 0
	}

	if alloc.IsDeficient != f.lastAllocation.IsDeficient ||
		alloc.PauseReason != f.lastAllocation.PauseReason ||
		alloc.TargetLayer != f.lastAllocation.TargetLayer ||
		alloc.RequestLayerSpatial != f.lastAllocation.RequestLayerSpatial {
		f.logger.Debugw(fmt.Sprintf("stream allocation: %s", reason), "allocation", &alloc)
	}
	f.lastAllocation = alloc

	f.setTargetLayer(f.lastAllocation.TargetLayer, f.lastAllocation.RequestLayerSpatial)
	if !f.vls.GetTarget().IsValid() {
		f.resyncLocked()
	}

	return f.lastAllocation
}

func (f *Forwarder) setTargetLayer(targetLayer buffer.VideoLayer, requestLayerSpatial int32) {
	f.vls.SetTarget(targetLayer)
	if targetLayer.IsValid() {
		f.vls.SetRequestSpatial(requestLayerSpatial)
	} else {
		f.vls.SetRequestSpatial(buffer.InvalidLayerSpatial)
	}
}

func (f *Forwarder) Resync() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.resyncLocked()
}

func (f *Forwarder) resyncLocked() {
	f.vls.SetCurrent(buffer.InvalidLayer)
	f.lastSSRC = 0
	if f.pubMuted {
		f.resumeBehindThreshold = ResumeBehindThresholdSeconds
	}
}

func (f *Forwarder) CheckSync() (bool, int32) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.vls.CheckSync()
}

func (f *Forwarder) FilterRTX(nacks []uint16) (filtered []uint16, disallowedLayers [buffer.DefaultMaxLayerSpatial + 1]bool) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if !FlagFilterRTX {
		filtered = nacks
	} else {
		filtered = f.rtpMunger.FilterRTX(nacks)
	}

	//
	// Curb RTX when deficient for two cases
	//   1. Target layer is lower than current layer. When current hits target, a key frame should flush the decoder.
	//   2. Requested layer is higher than current. Current layer's key frame should have flushed encoder.
	//      Remote might ask for older layer because of its jitter buffer, but let it starve as channel is already congested.
	//
	// Without the curb, when congestion hits, RTX rate could be so high that it further congests the channel.
	//
	if FlagFilterRTXLayers {
		currentLayer := f.vls.GetCurrent()
		targetLayer := f.vls.GetTarget()
		for layer := int32(0); layer < buffer.DefaultMaxLayerSpatial+1; layer++ {
			if f.isDeficientLocked() && (targetLayer.Spatial < currentLayer.Spatial || layer > currentLayer.Spatial) {
				disallowedLayers[layer] = true
			}
		}
	}
	return
}

func (f *Forwarder) GetTranslationParams(extPkt *buffer.ExtPacket, layer int32) (TranslationParams, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted || f.pubMuted {
		return TranslationParams{
			shouldDrop: true,
		}, nil
	}

	switch f.kind {
	case webrtc.RTPCodecTypeAudio:
		return f.getTranslationParamsAudio(extPkt, layer)
	case webrtc.RTPCodecTypeVideo:
		return f.getTranslationParamsVideo(extPkt, layer)
	}

	return TranslationParams{
		shouldDrop: true,
	}, ErrUnknownKind
}

func (f *Forwarder) processSourceSwitch(extPkt *buffer.ExtPacket, layer int32) error {
	if !f.started {
		f.started = true
		f.referenceLayerSpatial = layer
		f.rtpMunger.SetLastSnTs(extPkt)
		f.codecMunger.SetLast(extPkt)
		f.logger.Debugw(
			"starting forwarding",
			"sequenceNumber", extPkt.Packet.SequenceNumber,
			"extSequenceNumber", extPkt.ExtSequenceNumber,
			"timestamp", extPkt.Packet.Timestamp,
			"extTimestamp", extPkt.ExtTimestamp,
			"layer", layer,
			"referenceLayerSpatial", f.referenceLayerSpatial,
		)
		return nil
	} else if f.referenceLayerSpatial == buffer.InvalidLayerSpatial {
		f.referenceLayerSpatial = layer
		f.logger.Debugw(
			"catch up forwarding",
			"sequenceNumber", extPkt.Packet.SequenceNumber,
			"extSequenceNumber", extPkt.ExtSequenceNumber,
			"timestamp", extPkt.Packet.Timestamp,
			"extTimestamp", extPkt.ExtTimestamp,
			"layer", layer,
			"referenceLayerSpatial", f.referenceLayerSpatial,
		)
	}

	logTransition := func(message string, extExpectedTS, extRefTS, extLastTS uint64, diffSeconds float64) {
		f.logger.Debugw(
			message,
			"layer", layer,
			"extExpectedTS", extExpectedTS,
			"extRefTS", extRefTS,
			"extLastTS", extLastTS,
			"diffSeconds", math.Abs(diffSeconds),
		)
	}

	// Compute how much time passed between the previous forwarded packet
	// and the current incoming (to be forwarded) packet and calculate
	// timestamp offset on source change.
	//
	// There are three timestamps to consider here
	//   1. extLastTS -> timestamp of last sent packet
	//   2. extRefTS -> timestamp of this packet (after munging) calculated using feed's RTCP sender report
	//   3. extExpectedTS -> expected timestamp of this packet calculated based on elapsed time since first packet
	// Ideally, extRefTS and extExpectedTS should be very close and extLastTS should be before both of those.
	// But, cases like muting/unmuting, clock vagaries, pacing, etc. make them not satisfy those conditions always.
	rtpMungerState := f.rtpMunger.GetLast()
	extLastTS := rtpMungerState.ExtLastTS
	extExpectedTS := extLastTS
	extRefTS := extExpectedTS
	switchingAt := time.Now()
	if f.getReferenceLayerRTPTimestamp != nil {
		ts, err := f.getReferenceLayerRTPTimestamp(extPkt.Packet.Timestamp, layer, f.referenceLayerSpatial)
		if err != nil {
			// error out if extRefTS is not available. It can happen when there is no sender report
			// for the layer being switched to. Can especially happen at the start of the track when layer switches are
			// potentially happening very quickly. Erroring out and waiting for a layer for which a sender report has been
			// received will calculate a better offset, but may result in initial adaptation to take a bit longer depending
			// on how often publisher/remote side sends RTCP sender report.
			return err
		}

		extRefTS = (extRefTS & 0xFFFF_FFFF_0000_0000) + uint64(ts)

		expectedTS32 := uint32(extExpectedTS)
		if (ts-expectedTS32) < 1<<31 && ts < expectedTS32 {
			extRefTS += (1 << 32)
		}
		if (expectedTS32-ts) < 1<<31 && expectedTS32 < ts && extRefTS >= 1<<32 {
			extRefTS -= (1 << 32)
		}
	}

	if f.getExpectedRTPTimestamp != nil {
		tsExt, err := f.getExpectedRTPTimestamp(switchingAt)
		if err == nil {
			extExpectedTS = tsExt
		} else {
			if !f.preStartTime.IsZero() {
				timeSinceFirst := time.Since(f.preStartTime)
				rtpDiff := uint64(timeSinceFirst.Nanoseconds() * int64(f.codec.ClockRate) / 1e9)
				extExpectedTS = f.extFirstTS + rtpDiff
				if f.refTSOffset == 0 {
					f.refTSOffset = extExpectedTS - extRefTS
					f.logger.Infow(
						"calculating refTSOffset",
						"preStartTime", f.preStartTime.String(),
						"extFirstTS", f.extFirstTS,
						"timeSinceFirst", timeSinceFirst,
						"rtpDiff", rtpDiff,
						"extRefTS", extRefTS,
						"refTSOffset", f.refTSOffset,
					)
				}
			}
		}
	}
	extRefTS += f.refTSOffset

	var extNextTS uint64
	if f.lastSSRC == 0 {
		// If resuming (e. g. on unmute), keep next timestamp close to expected timestamp.
		//
		// Rationale:
		// Case 1: If mute is implemented via something like stopping a track and resuming it on unmute,
		// the RTP timestamp may not have jumped across mute valley. In this case, old timestamp
		// should not be used.
		//
		// Case 2: OTOH, something like pacing may be adding latency in the publisher path (even if
		// the timestamps incremented correctly across the mute valley). In this case, reference
		// timestamp should be used as things will catch up to real time when channel capacity
		// increases and pacer starts sending at faster rate.
		//
		// But, the challenege is distinguishing between the two cases. As a compromise, the difference
		// between extExpectedTS and extRefTS is thresholded. Difference below the threshold is treated as Case 2
		// and above as Case 1.
		//
		// In the event of extRefTS > extExpectedTS, use extRefTS.
		// Ideally, extRefTS should not be ahead of extExpectedTS, but extExpectedTS uses the first packet's
		// wall clock time. So, if the first packet experienced abmormal latency, it is possible
		// for extRefTS > extExpectedTS
		diffSeconds := float64(int64(extExpectedTS-extRefTS)) / float64(f.codec.ClockRate)
		if diffSeconds >= 0.0 {
			if f.resumeBehindThreshold > 0 && diffSeconds > f.resumeBehindThreshold {
				logTransition("resume, reference too far behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
				extNextTS = extExpectedTS
			} else if diffSeconds > ResumeBehindHighTresholdSeconds {
				// could be due to incorrect reference calculation
				logTransition("resume, reference very far behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
				extNextTS = extExpectedTS
			} else {
				extNextTS = extRefTS
			}
		} else {
			if math.Abs(diffSeconds) > SwitchAheadThresholdSeconds {
				logTransition("resume, reference too far ahead", extExpectedTS, extRefTS, extLastTS, diffSeconds)
			}
			extNextTS = extRefTS
		}
		f.resumeBehindThreshold = 0.0
	} else {
		// switching between layers, check if extRefTS is too far behind the last sent
		diffSeconds := float64(int64(extRefTS-extLastTS)) / float64(f.codec.ClockRate)
		if diffSeconds < 0.0 {
			if math.Abs(diffSeconds) > LayerSwitchBehindThresholdSeconds {
				// this could be due to pacer trickling out this layer. Error out and wait for a more opportune time.
				// AVSYNC-TODO: Consider some forcing function to do the switch
				// (like "have waited for too long for layer switch, nothing available, switch to whatever is available" kind of condition).
				logTransition("layer switch, reference too far behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
				return errors.New("switch point too far behind")
			}
			// use a nominal increase to ensure that timestamp is always moving forward
			logTransition("layer switch, reference is slightly behind", extExpectedTS, extRefTS, extLastTS, diffSeconds)
			extNextTS = extLastTS + 1
		} else {
			diffSeconds = float64(int64(extExpectedTS-extRefTS)) / float64(f.codec.ClockRate)
			if diffSeconds < 0.0 && math.Abs(diffSeconds) > SwitchAheadThresholdSeconds {
				logTransition("layer switch, reference too far ahead", extExpectedTS, extRefTS, extLastTS, diffSeconds)
			}
			extNextTS = extRefTS
		}
	}

	if int64(extNextTS-extLastTS) <= 0 {
		f.logger.Debugw("next timestamp is before last, adjusting", "extNextTS", extNextTS, "extLastTS", extLastTS)
		// nominal increase
		extNextTS = extLastTS + 1
	}
	f.logger.Debugw(
		"next timestamp on switch",
		"switchingAt", switchingAt.String(),
		"layer", layer,
		"extLastTS", extLastTS,
		"extRefTS", extRefTS,
		"refTSOffset", f.refTSOffset,
		"referenceLayerSpatial", f.referenceLayerSpatial,
		"extExpectedTS", extExpectedTS,
		"extNextTS", extNextTS,
		"tsJump", extNextTS-extLastTS,
		"nextSN", rtpMungerState.ExtLastSN+1,
		"extIncomingSN", extPkt.ExtSequenceNumber,
		"extIncomingTS", extPkt.ExtTimestamp,
	)

	f.rtpMunger.UpdateSnTsOffsets(extPkt, 1, extNextTS-extLastTS)
	f.codecMunger.UpdateOffsets(extPkt)
	return nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsCommon(extPkt *buffer.ExtPacket, layer int32, tp *TranslationParams) error {
	if f.lastSSRC != extPkt.Packet.SSRC {
		if err := f.processSourceSwitch(extPkt, layer); err != nil {
			tp.shouldDrop = true
			return nil
		}
		f.logger.Debugw("switching feed", "from", f.lastSSRC, "to", extPkt.Packet.SSRC)
		f.lastSSRC = extPkt.Packet.SSRC
	}

	tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt, tp.marker)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			return nil
		}
		return err
	}

	tp.rtp = tpRTP
	return nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsAudio(extPkt *buffer.ExtPacket, layer int32) (TranslationParams, error) {
	tp := TranslationParams{}
	if err := f.getTranslationParamsCommon(extPkt, layer, &tp); err != nil {
		tp.shouldDrop = true
		return tp, err
	}
	return tp, nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (TranslationParams, error) {
	maybeRollback := func(isSwitching bool) {
		if isSwitching {
			f.vls.Rollback()
		}
	}

	tp := TranslationParams{}
	if !f.vls.GetTarget().IsValid() {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	result := f.vls.Select(extPkt, layer)
	if !result.IsSelected {
		tp.shouldDrop = true
		if f.started && result.IsRelevant {
			// call to update highest incoming sequence number and other internal structures
			if tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt, result.RTPMarker); err == nil {
				if tpRTP.snOrdering == SequenceNumberOrderingContiguous {
					f.rtpMunger.PacketDropped(extPkt)
				}
			}
		}
		return tp, nil
	}
	tp.isResuming = result.IsResuming
	tp.isSwitching = result.IsSwitching
	tp.ddBytes = result.DependencyDescriptorExtension
	tp.marker = result.RTPMarker

	if FlagPauseOnDowngrade && f.isDeficientLocked() && f.vls.GetTarget().Spatial < f.vls.GetCurrent().Spatial {
		//
		// If target layer is lower than both the current and
		// maximum subscribed layer, it is due to bandwidth
		// constraints that the target layer has been switched down.
		// Continuing to send higher layer will only exacerbate the
		// situation by putting more stress on the channel. So, drop it.
		//
		// In the other direction, it is okay to keep forwarding till
		// switch point to get a smoother stream till the higher
		// layer key frame arrives.
		//
		// Note that it is possible for client subscription layer restriction
		// to coincide with server restriction due to bandwidth limitation,
		// In the case of subscription change, higher should continue streaming
		// to ensure smooth transition.
		//
		// To differentiate between the two cases, drop only when in DEFICIENT state.
		//
		tp.shouldDrop = true
		maybeRollback(result.IsSwitching)
		return tp, nil
	}

	err := f.getTranslationParamsCommon(extPkt, layer, &tp)
	if tp.shouldDrop {
		maybeRollback(result.IsSwitching)
		return tp, err
	}

	return tp, nil
}

func (f *Forwarder) TranslateCodecHeader(extPkt *buffer.ExtPacket, tpr *TranslationParamsRTP, outputBuffer []byte) (bool, int, int, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	maybeRollback := func(isSwitching bool) {
		if isSwitching {
			f.vls.Rollback()
		}
	}

	// codec specific forwarding check and any needed packet munging
	tl, isSwitching := f.vls.SelectTemporal(extPkt)
	inputSize, outputSize, err := f.codecMunger.UpdateAndGet(
		extPkt,
		tpr.snOrdering == SequenceNumberOrderingOutOfOrder,
		tpr.snOrdering == SequenceNumberOrderingGap,
		tl,
		outputBuffer,
	)
	if err != nil {
		if err == codecmunger.ErrFilteredVP8TemporalLayer || err == codecmunger.ErrOutOfOrderVP8PictureIdCacheMiss {
			if err == codecmunger.ErrFilteredVP8TemporalLayer {
				// filtered temporal layer, update sequence number offset to prevent holes
				f.rtpMunger.PacketDropped(extPkt)
			}
			maybeRollback(isSwitching)
			return false, 0, 0, nil
		}

		maybeRollback(isSwitching)
		return false, 0, 0, err
	}

	return true, inputSize, outputSize, nil
}

func (f *Forwarder) maybeStart() {
	if f.started {
		return
	}

	f.started = true
	f.preStartTime = time.Now()

	sequenceNumber := uint16(rand.Intn(1<<14)) + uint16(1<<15) // a random number in third quartile of sequence number space
	timestamp := uint32(rand.Intn(1<<30)) + uint32(1<<31)      // a random number in third quartile of timestamp space
	extPkt := &buffer.ExtPacket{
		Packet: &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: sequenceNumber,
				Timestamp:      timestamp,
			},
		},
		ExtSequenceNumber: uint64(sequenceNumber),
		ExtTimestamp:      uint64(timestamp),
	}
	f.rtpMunger.SetLastSnTs(extPkt)

	f.extFirstTS = uint64(timestamp)
	f.logger.Infow(
		"starting with dummy forwarding",
		"sequenceNumber", extPkt.Packet.SequenceNumber,
		"timestamp", extPkt.Packet.Timestamp,
		"preStartTime", f.preStartTime,
	)
}

func (f *Forwarder) GetSnTsForPadding(num int, forceMarker bool) ([]SnTs, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.maybeStart()

	// padding is used for probing. Padding packets should only
	// be at frame boundaries to ensure decoder sequencer does
	// not get out-of-sync. But, when a stream is paused,
	// force a frame marker as a restart of the stream will
	// start with a key frame which will reset the decoder.
	if !f.vls.GetTarget().IsValid() {
		forceMarker = true
	}
	return f.rtpMunger.UpdateAndGetPaddingSnTs(num, 0, 0, forceMarker, 0)
}

func (f *Forwarder) GetSnTsForBlankFrames(frameRate uint32, numPackets int) ([]SnTs, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.maybeStart()

	frameEndNeeded := !f.rtpMunger.IsOnFrameBoundary()
	if frameEndNeeded {
		numPackets++
	}

	extLastTS := f.rtpMunger.GetLast().ExtLastTS
	extExpectedTS := extLastTS
	if f.getExpectedRTPTimestamp != nil {
		tsExt, err := f.getExpectedRTPTimestamp(time.Now())
		if err == nil {
			extExpectedTS = tsExt
		}
	}
	if int64(extExpectedTS-extLastTS) <= 0 {
		extExpectedTS = extLastTS + 1
	}
	snts, err := f.rtpMunger.UpdateAndGetPaddingSnTs(numPackets, f.codec.ClockRate, frameRate, frameEndNeeded, extExpectedTS)
	return snts, frameEndNeeded, err
}

func (f *Forwarder) GetPadding(frameEndNeeded bool, outputBuffer []byte) (int, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.codecMunger.UpdateAndGetPadding(!frameEndNeeded, outputBuffer)
}

func (f *Forwarder) RTPMungerDebugInfo() map[string]interface{} {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.DebugInfo()
}

// -----------------------------------------------------------------------------

func getOptimalBandwidthNeeded(muted bool, pubMuted bool, maxPublishedLayer int32, brs Bitrates, maxLayer buffer.VideoLayer) int64 {
	if muted || pubMuted || maxPublishedLayer == buffer.InvalidLayerSpatial {
		return 0
	}

	for i := maxLayer.Spatial; i >= 0; i-- {
		for j := maxLayer.Temporal; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			return brs[i][j]
		}
	}

	// could be 0 due to either
	//   1. publisher has stopped all layers ==> feed dry.
	//   2. stream tracker has declared all layers stopped, functionally same as above.
	//      But, listed differently as this could be a mis-detection.
	//   3. Bitrate measurement is pending.
	return 0
}

func getBandwidthNeeded(brs Bitrates, layer buffer.VideoLayer, fallback int64) int64 {
	if layer.IsValid() && brs[layer.Spatial][layer.Temporal] > 0 {
		return brs[layer.Spatial][layer.Temporal]
	}

	return fallback
}

func getDistanceToDesired(
	muted bool,
	pubMuted bool,
	maxSeenLayer buffer.VideoLayer,
	availableLayers []int32,
	brs Bitrates,
	targetLayer buffer.VideoLayer,
	maxLayer buffer.VideoLayer,
) float64 {
	if muted || pubMuted || !maxSeenLayer.IsValid() || !maxLayer.IsValid() {
		return 0.0
	}

	adjustedMaxLayer := maxLayer

	maxAvailableSpatial := buffer.InvalidLayerSpatial
	maxAvailableTemporal := buffer.InvalidLayerTemporal

	// max available spatial is min(subscribedMax, publishedMax, availableMax)
	// subscribedMax = subscriber requested max spatial layer
	// publishedMax = max spatial layer ever published
	// availableMax = based on bit rate measurement, available max spatial layer
done:
	for s := int32(len(brs)) - 1; s >= 0; s-- {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[s][t] != 0 {
				maxAvailableSpatial = s
				break done
			}
		}
	}

	// before bit rate measurement is available, stream tracker could declare layer seen, account for that
	for _, layer := range availableLayers {
		if layer > maxAvailableSpatial {
			maxAvailableSpatial = layer
			maxAvailableTemporal = maxSeenLayer.Temporal // till bit rate measurement is available, assume max seen as temporal
		}
	}

	if maxAvailableSpatial < adjustedMaxLayer.Spatial {
		adjustedMaxLayer.Spatial = maxAvailableSpatial
	}

	if maxSeenLayer.Spatial < adjustedMaxLayer.Spatial {
		adjustedMaxLayer.Spatial = maxSeenLayer.Spatial
	}

	// max available temporal is min(subscribedMax, temporalLayerSeenMax, availableMax)
	// subscribedMax = subscriber requested max temporal layer
	// temporalLayerSeenMax = max temporal layer ever published/seen
	// availableMax = based on bit rate measurement, available max temporal in the adjusted max spatial layer
	if adjustedMaxLayer.Spatial != buffer.InvalidLayerSpatial {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[adjustedMaxLayer.Spatial][t] != 0 {
				maxAvailableTemporal = t
				break
			}
		}
	}
	if maxAvailableTemporal < adjustedMaxLayer.Temporal {
		adjustedMaxLayer.Temporal = maxAvailableTemporal
	}

	if maxSeenLayer.Temporal < adjustedMaxLayer.Temporal {
		adjustedMaxLayer.Temporal = maxSeenLayer.Temporal
	}

	if !adjustedMaxLayer.IsValid() {
		adjustedMaxLayer = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	// adjust target layers if they are invalid, i. e. not streaming
	adjustedTargetLayer := targetLayer
	if !targetLayer.IsValid() {
		adjustedTargetLayer = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	distance :=
		((adjustedMaxLayer.Spatial - adjustedTargetLayer.Spatial) * (maxSeenLayer.Temporal + 1)) +
			(adjustedMaxLayer.Temporal - adjustedTargetLayer.Temporal)
	if !targetLayer.IsValid() {
		distance += (maxSeenLayer.Temporal + 1)
	}

	return float64(distance) / float64(maxSeenLayer.Temporal+1)
}
