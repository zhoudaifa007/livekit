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

package buffer

import (
	"fmt"
	"math"
	"time"

	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/livekit"
	protoutils "github.com/livekit/protocol/utils"
)

const (
	cHistorySize = 4096

	// RTCP Sender Reports are re-based to SFU time base so that all subscriber side
	// can have the same time base (i. e. SFU time base). To convert publisher side
	// RTCP Sender Reports to SFU timebase, a propagation delay is maintained.
	//    propagation_delay = time_of_report_reception - ntp_timestamp_in_report
	//
	// Propagation delay is adapted continuously. If it falls, adapt quickly to the
	// lower value as that could be the real propagation delay. If it rises, adapt slowly
	// as it might be a temporary change or slow drift. See below for handling of high deltas
	// which could be a result of a path change.
	cPropagationDelayFallFactor = float64(0.95)
	cPropagationDelayRiseFactor = float64(0.05)

	cPropagationDelaySpikeAdaptationFactor = float64(0.5)

	// do not adapt to small OR large (outlier) changes
	cPropagationDelayDeltaThresholdMin       = 5 * time.Millisecond
	cPropagationDelayDeltaThresholdMaxFactor = 2

	// To account for path changes mid-stream, if the delta of the propagation delay is consistently higher, reset.
	// Reset at whichever of the below happens later.
	//
	// A long term version of delta of propagation delay is maintained and delta propagation delay exceeding
	// a factor of the long term version is considered a sharp increase. That will trigger the start of the
	// path change condition and if it persists, propagation delay will be reset.
	cPropagationDelayDeltaMaxInterval         = 10 * time.Second
	cPropagationDelayDeltaHighResetNumReports = 3
	cPropagationDelayDeltaHighResetWait       = 10 * time.Second
)

type RTPFlowState struct {
	IsNotHandled bool

	HasLoss            bool
	LossStartInclusive uint64
	LossEndExclusive   uint64

	IsDuplicate  bool
	IsOutOfOrder bool

	ExtSequenceNumber uint64
	ExtTimestamp      uint64
}

type RTPStatsReceiver struct {
	*rtpStatsBase

	sequenceNumber *utils.WrapAround[uint16, uint64]

	timestamp *utils.WrapAround[uint32, uint64]

	history *protoutils.Bitmap[uint64]

	propagationDelay                   time.Duration
	longTermDeltaPropagationDelay      time.Duration
	propagationDelayDeltaHighCount     int
	propagationDelayDeltaHighStartTime time.Time
	propagationDelaySpike              time.Duration

	clockSkewCount               int
	outOfOrderSsenderReportCount int
}

func NewRTPStatsReceiver(params RTPStatsParams) *RTPStatsReceiver {
	return &RTPStatsReceiver{
		rtpStatsBase:   newRTPStatsBase(params),
		sequenceNumber: utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		timestamp:      utils.NewWrapAround[uint32, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		history:        protoutils.NewBitmap[uint64](cHistorySize),
	}
}

func (r *RTPStatsReceiver) NewSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.newSnapshotID(r.sequenceNumber.GetExtendedHighest())
}

func (r *RTPStatsReceiver) Update(
	packetTime time.Time,
	sequenceNumber uint16,
	timestamp uint32,
	marker bool,
	hdrSize int,
	payloadSize int,
	paddingSize int,
) (flowState RTPFlowState) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		flowState.IsNotHandled = true
		return
	}

	var resSN utils.WrapAroundUpdateResult[uint64]
	var resTS utils.WrapAroundUpdateResult[uint64]
	if !r.initialized {
		if payloadSize == 0 {
			// do not start on a padding only packet
			flowState.IsNotHandled = true
			return
		}

		r.initialized = true

		r.startTime = time.Now()

		r.firstTime = packetTime
		r.highestTime = packetTime

		resSN = r.sequenceNumber.Update(sequenceNumber)
		resTS = r.timestamp.Update(timestamp)

		// initialize snapshots if any
		for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
			r.snapshots[i] = r.initSnapshot(r.startTime, r.sequenceNumber.GetExtendedStart())
		}

		r.logger.Debugw(
			"rtp receiver stream start",
			"startTime", r.startTime.String(),
			"firstTime", r.firstTime.String(),
			"startSN", r.sequenceNumber.GetExtendedStart(),
			"startTS", r.timestamp.GetExtendedStart(),
		)
	} else {
		resSN = r.sequenceNumber.Update(sequenceNumber)
		if resSN.IsUnhandled {
			flowState.IsNotHandled = true
			return
		}
		resTS = r.timestamp.Update(timestamp)
	}

	pktSize := uint64(hdrSize + payloadSize + paddingSize)
	gapSN := int64(resSN.ExtendedVal - resSN.PreExtendedHighest)
	if gapSN <= 0 { // duplicate OR out-of-order
		if -gapSN >= cNumSequenceNumbers/2 {
			r.logger.Warnw(
				"large sequence number gap negative", nil,
				"extStartSN", r.sequenceNumber.GetExtendedStart(),
				"extHighestSN", r.sequenceNumber.GetExtendedHighest(),
				"extStartTS", r.timestamp.GetExtendedStart(),
				"extHighestTS", r.timestamp.GetExtendedHighest(),
				"firstTime", r.firstTime.String(),
				"highestTime", r.highestTime.String(),
				"prev", resSN.PreExtendedHighest,
				"curr", resSN.ExtendedVal,
				"gap", gapSN,
				"packetTime", packetTime.String(),
				"sequenceNumber", sequenceNumber,
				"timestamp", timestamp,
				"marker", marker,
				"hdrSize", hdrSize,
				"payloadSize", payloadSize,
				"paddingSize", paddingSize,
			)
		}

		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if r.isInRange(resSN.ExtendedVal, resSN.PreExtendedHighest) {
			if r.history.IsSet(resSN.ExtendedVal) {
				r.bytesDuplicate += pktSize
				r.headerBytesDuplicate += uint64(hdrSize)
				r.packetsDuplicate++
				flowState.IsDuplicate = true
			} else {
				r.packetsLost--
				r.history.Set(resSN.ExtendedVal)
			}
		}

		flowState.IsOutOfOrder = true
		flowState.ExtSequenceNumber = resSN.ExtendedVal
		flowState.ExtTimestamp = resTS.ExtendedVal
	} else { // in-order
		if gapSN >= cNumSequenceNumbers/2 {
			r.logger.Warnw(
				"large sequence number gap", nil,
				"extStartSN", r.sequenceNumber.GetExtendedStart(),
				"extHighestSN", r.sequenceNumber.GetExtendedHighest(),
				"extStartTS", r.timestamp.GetExtendedStart(),
				"extHighestTS", r.timestamp.GetExtendedHighest(),
				"firstTime", r.firstTime.String(),
				"highestTime", r.highestTime.String(),
				"prev", resSN.PreExtendedHighest,
				"curr", resSN.ExtendedVal,
				"gap", gapSN,
				"packetTime", packetTime.String(),
				"sequenceNumber", sequenceNumber,
				"timestamp", timestamp,
				"marker", marker,
				"hdrSize", hdrSize,
				"payloadSize", payloadSize,
				"paddingSize", paddingSize,
			)
		}

		// update gap histogram
		r.updateGapHistogram(int(gapSN))

		// update missing sequence numbers
		r.history.ClearRange(resSN.PreExtendedHighest+1, resSN.ExtendedVal-1)
		r.packetsLost += uint64(gapSN - 1)

		r.history.Set(resSN.ExtendedVal)

		if timestamp != uint32(resTS.PreExtendedHighest) {
			// update only on first packet as same timestamp could be in multiple packets.
			// NOTE: this may not be the first packet with this time stamp if there is packet loss.
			r.highestTime = packetTime
		}

		if gapSN > 1 {
			flowState.HasLoss = true
			flowState.LossStartInclusive = resSN.PreExtendedHighest + 1
			flowState.LossEndExclusive = resSN.ExtendedVal
		}
		flowState.ExtSequenceNumber = resSN.ExtendedVal
		flowState.ExtTimestamp = resTS.ExtendedVal
	}

	if !flowState.IsDuplicate {
		if payloadSize == 0 {
			r.packetsPadding++
			r.bytesPadding += pktSize
			r.headerBytesPadding += uint64(hdrSize)
		} else {
			r.bytes += pktSize
			r.headerBytes += uint64(hdrSize)

			if marker {
				r.frames++
			}

			r.updateJitter(resTS.ExtendedVal, packetTime)
		}
	}
	return
}

func (r *RTPStatsReceiver) SetRtcpSenderReportData(srData *RTCPSenderReportData) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if srData == nil || !r.initialized {
		return
	}

	// prevent against extreme case of anachronous sender reports
	if r.srNewest != nil && r.srNewest.NTPTimestamp > srData.NTPTimestamp {
		r.logger.Infow(
			"received sender report, anachronous, dropping",
			"first", r.srFirst,
			"last", r.srNewest,
			"current", srData,
		)
		return
	}

	tsCycles := uint64(0)
	if r.srNewest != nil {
		tsCycles = r.srNewest.RTPTimestampExt & 0xFFFF_FFFF_0000_0000
		if (srData.RTPTimestamp-r.srNewest.RTPTimestamp) < (1<<31) && srData.RTPTimestamp < r.srNewest.RTPTimestamp {
			tsCycles += (1 << 32)
		}

		if tsCycles >= (1 << 32) {
			if (srData.RTPTimestamp-r.srNewest.RTPTimestamp) >= (1<<31) && srData.RTPTimestamp > r.srNewest.RTPTimestamp {
				tsCycles -= (1 << 32)
			}
		}
	}

	srDataCopy := *srData
	srDataCopy.RTPTimestampExt = uint64(srDataCopy.RTPTimestamp) + tsCycles

	if r.srNewest != nil && srDataCopy.RTPTimestampExt < r.srNewest.RTPTimestampExt {
		// This can happen when a track is replaced with a null and then restored -
		// i. e. muting replacing with null and unmute restoring the original track.
		// Or it could be due bad report generation.
		// In any case, ignore out-of-order reports.
		if r.outOfOrderSsenderReportCount%10 == 0 {
			r.logger.Infow(
				"received sender report, out-of-order, skipping",
				"first", r.srFirst,
				"last", r.srNewest,
				"current", &srDataCopy,
				"count", r.outOfOrderSsenderReportCount,
			)
		}
		r.outOfOrderSsenderReportCount++
		return
	}

	if r.srNewest != nil {
		timeSinceLast := srData.NTPTimestamp.Time().Sub(r.srNewest.NTPTimestamp.Time()).Seconds()
		rtpDiffSinceLast := srDataCopy.RTPTimestampExt - r.srNewest.RTPTimestampExt
		calculatedClockRateFromLast := float64(rtpDiffSinceLast) / timeSinceLast

		timeSinceFirst := srData.NTPTimestamp.Time().Sub(r.srFirst.NTPTimestamp.Time()).Seconds()
		rtpDiffSinceFirst := srDataCopy.RTPTimestampExt - r.srFirst.RTPTimestampExt
		calculatedClockRateFromFirst := float64(rtpDiffSinceFirst) / timeSinceFirst

		if (timeSinceLast > 0.2 && math.Abs(float64(r.params.ClockRate)-calculatedClockRateFromLast) > 0.2*float64(r.params.ClockRate)) ||
			(timeSinceFirst > 0.2 && math.Abs(float64(r.params.ClockRate)-calculatedClockRateFromFirst) > 0.2*float64(r.params.ClockRate)) {
			if r.clockSkewCount%100 == 0 {
				r.logger.Infow(
					"received sender report, clock skew",
					"first", r.srFirst,
					"last", r.srNewest,
					"current", &srDataCopy,
					"timeSinceFirst", timeSinceFirst,
					"rtpDiffSinceFirst", rtpDiffSinceFirst,
					"calculatedFirst", calculatedClockRateFromFirst,
					"timeSinceLast", timeSinceLast,
					"rtpDiffSinceLast", rtpDiffSinceLast,
					"calculatedLast", calculatedClockRateFromLast,
					"count", r.clockSkewCount,
				)
			}
			r.clockSkewCount++
		}
	}

	var propagationDelay time.Duration
	var deltaPropagationDelay time.Duration
	getPropagationFields := func() []interface{} {
		return []interface{}{
			"propagationDelay", r.propagationDelay.String(),
			"receivedPropagationDelay", propagationDelay.String(),
			"longTermDeltaPropagationDelay", r.longTermDeltaPropagationDelay.String(),
			"receivedDeltaPropagationDelay", deltaPropagationDelay.String(),
			"deltaHighCount", r.propagationDelayDeltaHighCount,
			"sinceDeltaHighStart", time.Since(r.propagationDelayDeltaHighStartTime).String(),
			"first", r.srFirst,
			"last", r.srNewest,
			"current", &srDataCopy,
		}
	}
	resetDelta := func() {
		r.propagationDelayDeltaHighCount = 0
		r.propagationDelayDeltaHighStartTime = time.Time{}
		r.propagationDelaySpike = 0
	}
	initPropagationDelay := func(pd time.Duration) {
		r.propagationDelay = pd

		r.longTermDeltaPropagationDelay = 0

		resetDelta()
	}

	ntpTime := srDataCopy.NTPTimestamp.Time()
	propagationDelay = srDataCopy.At.Sub(ntpTime)
	if r.srFirst == nil {
		r.srFirst = &srDataCopy
		initPropagationDelay(propagationDelay)
		r.logger.Debugw("initializing propagation delay", getPropagationFields()...)
	} else {
		deltaPropagationDelay = propagationDelay - r.propagationDelay
		if deltaPropagationDelay.Abs() > cPropagationDelayDeltaThresholdMin { // ignore small changes
			if r.longTermDeltaPropagationDelay != 0 && deltaPropagationDelay > 0 && deltaPropagationDelay > r.longTermDeltaPropagationDelay*time.Duration(cPropagationDelayDeltaThresholdMaxFactor) {
				r.logger.Debugw("sharp increase in propagation delay, skipping", getPropagationFields()...) // TODO-REMOVE
				r.propagationDelayDeltaHighCount++
				if r.propagationDelayDeltaHighStartTime.IsZero() {
					r.propagationDelayDeltaHighStartTime = time.Now()
				}
				if r.propagationDelaySpike == 0 {
					r.propagationDelaySpike = propagationDelay
				} else {
					r.propagationDelaySpike += time.Duration(cPropagationDelaySpikeAdaptationFactor * float64(propagationDelay-r.propagationDelaySpike))
				}

				if r.propagationDelayDeltaHighCount >= cPropagationDelayDeltaHighResetNumReports && time.Since(r.propagationDelayDeltaHighStartTime) >= cPropagationDelayDeltaHighResetWait {
					r.logger.Debugw("re-initializing propagation delay", append(getPropagationFields(), "newPropagationDelay", propagationDelay.String())...)
					initPropagationDelay(r.propagationDelaySpike)
				}
			} else {
				resetDelta()

				if deltaPropagationDelay.Abs() > cPropagationDelayDeltaThresholdMin {
					factor := cPropagationDelayFallFactor
					if propagationDelay > r.propagationDelay {
						factor = cPropagationDelayRiseFactor
					}
					fields := append(
						getPropagationFields(),
						"adjustedPropagationDelay", r.propagationDelay+time.Duration(factor*float64(propagationDelay-r.propagationDelay)),
					) // TODO-REMOVE
					r.logger.Debugw("adapting propagation delay", fields...) // TODO-REMOVE
					r.propagationDelay += time.Duration(factor * float64(propagationDelay-r.propagationDelay))
				}
			}
		} else {
			r.propagationDelayDeltaHighCount = 0
			r.propagationDelayDeltaHighStartTime = time.Time{}
		}
		if r.longTermDeltaPropagationDelay == 0 {
			r.longTermDeltaPropagationDelay = deltaPropagationDelay
		} else {
			sinceLastReport := srDataCopy.NTPTimestamp.Time().Sub(r.srNewest.NTPTimestamp.Time())
			adaptationFactor := min(1.0, float64(sinceLastReport)/float64(cPropagationDelayDeltaMaxInterval))
			r.longTermDeltaPropagationDelay += time.Duration(adaptationFactor * float64(deltaPropagationDelay-r.longTermDeltaPropagationDelay))
		}
	}
	// adjust receive time to estimated propagation delay
	srDataCopy.At = ntpTime.Add(r.propagationDelay)
	r.srNewest = &srDataCopy

	r.maybeAdjustFirstPacketTime(r.srNewest, 0, r.timestamp.GetExtendedStart())
}

func (r *RTPStatsReceiver) GetRtcpSenderReportData() *RTCPSenderReportData {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.srNewest == nil {
		return nil
	}

	srNewestCopy := *r.srNewest
	return &srNewestCopy
}

func (r *RTPStatsReceiver) GetRtcpReceptionReport(ssrc uint32, proxyFracLost uint8, snapshotID uint32) *rtcp.ReceptionReport {
	r.lock.Lock()
	defer r.lock.Unlock()

	extHighestSN := r.sequenceNumber.GetExtendedHighest()
	then, now := r.getAndResetSnapshot(snapshotID, r.sequenceNumber.GetExtendedStart(), extHighestSN)
	if now == nil || then == nil {
		return nil
	}

	packetsExpected := now.extStartSN - then.extStartSN
	if packetsExpected > cNumSequenceNumbers {
		r.logger.Warnw(
			"too many packets expected in receiver report",
			fmt.Errorf("start: %d, end: %d, expected: %d", then.extStartSN, now.extStartSN, packetsExpected),
		)
		return nil
	}
	if packetsExpected == 0 {
		return nil
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}
	lossRate := float32(packetsLost) / float32(packetsExpected)
	fracLost := uint8(lossRate * 256.0)
	if proxyFracLost > fracLost {
		fracLost = proxyFracLost
	}

	totalLost := r.packetsLost
	if totalLost > 0xffffff { // 24-bits max
		totalLost = 0xffffff
	}

	lastSR := uint32(0)
	dlsr := uint32(0)
	if r.srNewest != nil {
		lastSR = uint32(r.srNewest.NTPTimestamp >> 16)
		if !r.srNewest.At.IsZero() {
			delayUS := time.Since(r.srNewest.At).Microseconds()
			dlsr = uint32(delayUS * 65536 / 1e6)
		}
	}

	return &rtcp.ReceptionReport{
		SSRC:               ssrc,
		FractionLost:       fracLost,
		TotalLost:          uint32(totalLost),
		LastSequenceNumber: uint32(now.extStartSN),
		Jitter:             uint32(r.jitter),
		LastSenderReport:   lastSR,
		Delay:              dlsr,
	}
}

func (r *RTPStatsReceiver) DeltaInfo(snapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.deltaInfo(snapshotID, r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest())
}

func (r *RTPStatsReceiver) String() string {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toString(
		r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest(), r.timestamp.GetExtendedStart(), r.timestamp.GetExtendedHighest(),
		r.packetsLost,
		r.jitter, r.maxJitter,
	)
}

func (r *RTPStatsReceiver) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toProto(
		r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest(), r.timestamp.GetExtendedStart(), r.timestamp.GetExtendedHighest(),
		r.packetsLost,
		r.jitter, r.maxJitter,
	)
}

func (r *RTPStatsReceiver) isInRange(esn uint64, ehsn uint64) bool {
	diff := int64(ehsn - esn)
	return diff >= 0 && diff < cHistorySize
}

// ----------------------------------
