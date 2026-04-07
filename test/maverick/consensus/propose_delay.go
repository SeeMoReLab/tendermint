package consensus

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	cstypes "github.com/tendermint/tendermint/consensus/types"
)

// DelayScheduleEntry defines a point in time at which propose delays are updated
// for specific nodes.
type DelayScheduleEntry struct {
	At    string            `json:"at"`    // duration from process start, e.g. "10s"
	Nodes map[string]string `json:"nodes"` // node index (as string) -> propose delay duration
}

// failureSpec mirrors the subset of failure_spec.xml we care about for
// Tendermint proposal-delay injection.
type failureSpec struct {
	WarmUpTime string         `xml:"warmUpTime"`
	Phases     []failurePhase `xml:"phases>phase"`
}

type failurePhase struct {
	AtTime     string            `xml:"atTime"`
	Time       string            `xml:"time"` // legacy duration-based field
	Tendermint failureTendermint `xml:"tendermint"`
}

type failureTendermint struct {
	ProposalDelay failureProposalDelay `xml:"proposalDelay"`
}

type failureProposalDelay struct {
	DelayMs  string             `xml:"delayMs"`
	Replicas failureReplicaList `xml:"replicas"`
}

type failureReplicaList struct {
	ReplicaEntries []failureReplica `xml:"replica"`
	IDs            []string         `xml:"id"` // legacy compact form
}

type failureReplica struct {
	ID      string `xml:"id"`
	DelayMs string `xml:"delayMs"`
}

type schedulePoint struct {
	FireAfter time.Duration
	Delay     time.Duration
}

// ProposeDelayMisbehavior reads a phase schedule file and returns a Misbehavior
// that dynamically updates the proposal delay for this node (identified by
// nodeIndex) at each scheduled time relative to process start.
//
// Supported formats:
//
//  1. Preferred: failure_spec.xml (<failureSpec><warmUpTime>...<phases>...)
//     where per-phase delay is under:
//     <phase><tendermint><proposalDelay><delayMs>...</delayMs>...
//
//  2. Backward compatibility: legacy JSON delay schedule:
//     [{"at":"10s","nodes":{"0":"4s"}}]
//
// A delay of "0s" disables the delay. If scheduleFile is empty, the misbehavior
// runs with no delay (behaves identically to the default).
//
// If failureStartUnixMs > 0, warm-up and phase timings are anchored to that
// absolute Unix timestamp (milliseconds). Otherwise they are anchored to
// process start time.
func ProposeDelayMisbehavior(scheduleFile string, nodeIndex int, failureStartUnixMs int64) (Misbehavior, error) {
	b := DefaultMisbehavior()
	b.Name = "propose-delay"

	// currentDelay holds a *time.Duration updated atomically.
	var currentDelay unsafe.Pointer
	zero := time.Duration(0)
	atomic.StorePointer(&currentDelay, unsafe.Pointer(&zero))

	if scheduleFile != "" {
		points, err := loadSchedulePoints(scheduleFile, nodeIndex)
		if err != nil {
			return b, err
		}
		start := time.Now()
		if failureStartUnixMs > 0 {
			start = time.Unix(0, failureStartUnixMs*int64(time.Millisecond))
		}
		for _, point := range points {
			go func(fireAfter, proposeDelay time.Duration) {
				time.Sleep(time.Until(start.Add(fireAfter)))
				atomic.StorePointer(&currentDelay, unsafe.Pointer(&proposeDelay))
			}(point.FireAfter, point.Delay)
		}
	}

	b.EnterPropose = func(cs *State, height int64, round int32) {
		// Always schedule the propose timeout so other nodes can progress if
		// the delay exceeds timeout_propose.
		cs.scheduleTimeout(cs.config.Propose(round), height, round, cstypes.RoundStepPropose)

		pubKey, err := cs.privValidator.GetPubKey()
		if err != nil {
			cs.Logger.Error("propose-delay: could not get pubkey", "err", err)
			return
		}
		if !cs.isProposer(pubKey.Address()) {
			return // not our turn to propose
		}

		d := *(*time.Duration)(atomic.LoadPointer(&currentDelay))
		if d <= 0 {
			cs.decideProposal(height, round)
			return
		}

		cs.Logger.Info("propose-delay: delaying proposal", "delay", d, "height", height, "round", round)
		go func() {
			time.Sleep(d)
			cs.decideProposal(height, round)
		}()
	}

	return b, nil
}

func loadSchedulePoints(scheduleFile string, nodeIndex int) ([]schedulePoint, error) {
	data, err := os.ReadFile(scheduleFile)
	if err != nil {
		return nil, fmt.Errorf("propose-delay: reading schedule file: %w", err)
	}

	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "<") {
		points, err := parseFailureSpecXML(data, nodeIndex)
		if err != nil {
			return nil, fmt.Errorf("propose-delay: parsing failure spec XML: %w", err)
		}
		return points, nil
	}

	points, err := parseLegacyJSONSchedule(data, nodeIndex)
	if err != nil {
		return nil, fmt.Errorf("propose-delay: parsing legacy JSON schedule: %w", err)
	}
	return points, nil
}

func parseLegacyJSONSchedule(data []byte, nodeIndex int) ([]schedulePoint, error) {
	var schedule []DelayScheduleEntry
	if err := json.Unmarshal(data, &schedule); err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%d", nodeIndex)
	points := make([]schedulePoint, 0, len(schedule))
	for _, entry := range schedule {
		delayStr, ok := entry.Nodes[key]
		if !ok {
			continue
		}
		fireAfter, err := time.ParseDuration(strings.TrimSpace(entry.At))
		if err != nil {
			return nil, fmt.Errorf("invalid 'at' duration %q: %w", entry.At, err)
		}
		proposeDelay, err := time.ParseDuration(strings.TrimSpace(delayStr))
		if err != nil {
			return nil, fmt.Errorf("invalid delay duration %q for node %s: %w", delayStr, key, err)
		}
		points = append(points, schedulePoint{FireAfter: fireAfter, Delay: proposeDelay})
	}

	sort.Slice(points, func(i, j int) bool { return points[i].FireAfter < points[j].FireAfter })
	return points, nil
}

func parseFailureSpecXML(data []byte, nodeIndex int) ([]schedulePoint, error) {
	var spec failureSpec
	if err := xml.Unmarshal(data, &spec); err != nil {
		return nil, err
	}

	warmUpDuration, err := parseSecondsDuration(spec.WarmUpTime)
	if err != nil {
		return nil, fmt.Errorf("invalid warmUpTime %q: %w", spec.WarmUpTime, err)
	}

	points := make([]schedulePoint, 0, len(spec.Phases))
	for _, phase := range spec.Phases {
		atText := strings.TrimSpace(phase.AtTime)
		if atText == "" {
			atText = strings.TrimSpace(phase.Time) // legacy fallback
		}
		atDuration, err := parseSecondsDuration(atText)
		if err != nil {
			return nil, fmt.Errorf("invalid phase atTime/time %q: %w", atText, err)
		}

		delay, ok, err := delayForNodeFromPhase(phase.Tendermint.ProposalDelay, nodeIndex)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		fireAfter := warmUpDuration + atDuration
		if fireAfter < 0 {
			fireAfter = 0
		}
		points = append(points, schedulePoint{FireAfter: fireAfter, Delay: delay})
	}

	sort.Slice(points, func(i, j int) bool { return points[i].FireAfter < points[j].FireAfter })
	return points, nil
}

func delayForNodeFromPhase(pd failureProposalDelay, nodeIndex int) (time.Duration, bool, error) {
	defaultDelayText := strings.TrimSpace(pd.DelayMs)
	hasDefault := defaultDelayText != ""

	// Preferred Tendermint style: one phase-level delay for all maverick nodes.
	if hasDefault && len(pd.Replicas.ReplicaEntries) == 0 && len(pd.Replicas.IDs) == 0 {
		delay, err := parseMillisecondsDuration(defaultDelayText)
		if err != nil {
			return 0, false, fmt.Errorf("invalid proposalDelay.delayMs %q: %w", defaultDelayText, err)
		}
		return delay, true, nil
	}

	nodeKey := strconv.Itoa(nodeIndex)

	// Expanded entry form: <replica><id>..</id><delayMs>..</delayMs></replica>
	for _, r := range pd.Replicas.ReplicaEntries {
		idText := strings.TrimSpace(r.ID)
		if idText == "" {
			continue
		}
		if !idMatchesNode(idText, nodeKey) {
			continue
		}
		delayText := strings.TrimSpace(r.DelayMs)
		if delayText == "" {
			delayText = defaultDelayText
		}
		if delayText == "" {
			return 0, false, nil
		}
		delay, err := parseMillisecondsDuration(delayText)
		if err != nil {
			return 0, false, fmt.Errorf("invalid replica delayMs %q for replica %q: %w", delayText, idText, err)
		}
		return delay, true, nil
	}

	// Legacy compact form: <replicas><id>0</id>...</replicas> + proposalDelay.delayMs
	if hasDefault {
		for _, idText := range pd.Replicas.IDs {
			if idMatchesNode(idText, nodeKey) {
				delay, err := parseMillisecondsDuration(defaultDelayText)
				if err != nil {
					return 0, false, fmt.Errorf("invalid proposalDelay.delayMs %q: %w", defaultDelayText, err)
				}
				return delay, true, nil
			}
		}
	}

	return 0, false, nil
}

func idMatchesNode(idText, nodeKey string) bool {
	id := strings.TrimSpace(idText)
	if id == "" {
		return false
	}
	// Friendly wildcard aliases.
	if strings.EqualFold(id, "all") || id == "*" {
		return true
	}
	return id == nodeKey
}

func parseSecondsDuration(text string) (time.Duration, error) {
	value := strings.TrimSpace(text)
	if value == "" {
		return 0, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if seconds < 0 {
		seconds = 0
	}
	return time.Duration(math.Round(seconds * float64(time.Second))), nil
}

func parseMillisecondsDuration(text string) (time.Duration, error) {
	value := strings.TrimSpace(text)
	if value == "" {
		return 0, nil
	}
	milliseconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if milliseconds < 0 {
		milliseconds = 0
	}
	return time.Duration(math.Round(milliseconds * float64(time.Millisecond))), nil
}
