package consensus

import (
	"encoding/json"
	"fmt"
	"os"
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

// ProposeDelayMisbehavior reads a JSON schedule file and returns a Misbehavior
// that dynamically updates the proposal delay for this node (identified by
// nodeIndex) at each scheduled time relative to process start.
//
// Schedule format:
//
//	[
//	  {"at": "10s", "nodes": {"0": "4s", "1": "2s"}},
//	  {"at": "40s", "nodes": {"0": "0s"}}
//	]
//
// A delay of "0s" disables the delay. If scheduleFile is empty, the misbehavior
// runs with no delay (behaves identically to the default).
func ProposeDelayMisbehavior(scheduleFile string, nodeIndex int) (Misbehavior, error) {
	b := DefaultMisbehavior()
	b.Name = "propose-delay"

	// currentDelay holds a *time.Duration updated atomically.
	var currentDelay unsafe.Pointer
	zero := time.Duration(0)
	atomic.StorePointer(&currentDelay, unsafe.Pointer(&zero))

	if scheduleFile != "" {
		data, err := os.ReadFile(scheduleFile)
		if err != nil {
			return b, fmt.Errorf("propose-delay: reading schedule file: %w", err)
		}
		var schedule []DelayScheduleEntry
		if err := json.Unmarshal(data, &schedule); err != nil {
			return b, fmt.Errorf("propose-delay: parsing schedule file: %w", err)
		}

		key := fmt.Sprintf("%d", nodeIndex)
		start := time.Now()
		for _, entry := range schedule {
			delayStr, ok := entry.Nodes[key]
			if !ok {
				continue // this entry does not apply to this node
			}
			fireAfter, err := time.ParseDuration(entry.At)
			if err != nil {
				return b, fmt.Errorf("propose-delay: parsing 'at' field %q: %w", entry.At, err)
			}
			proposeDelay, err := time.ParseDuration(delayStr)
			if err != nil {
				return b, fmt.Errorf("propose-delay: parsing delay %q for node %s: %w", delayStr, key, err)
			}
			go func(fireAfter, proposeDelay time.Duration) {
				time.Sleep(time.Until(start.Add(fireAfter)))
				atomic.StorePointer(&currentDelay, unsafe.Pointer(&proposeDelay))
			}(fireAfter, proposeDelay)
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
