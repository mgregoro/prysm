package stategen

import (
	"context"
	"encoding/hex"

	"github.com/pkg/errors"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/db/filters"
	"github.com/prysmaticlabs/prysm/beacon-chain/state"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

// This saves a post finalized beacon state in the hot section of the DB. On the epoch boundary,
// it saves a full state. On an intermediate slot, it saves a back pointer to the
// nearest epoch boundary state.
func (s *State) saveHotState(ctx context.Context, blockRoot [32]byte, state *state.BeaconState) error {
	ctx, span := trace.StartSpan(ctx, "stateGen.saveHotState")
	defer span.End()

	// If the hot state is already in cache, one can be sure the state was processed and in the DB.
	if s.hotStateCache.Has(blockRoot) {
		return nil
	}

	// Only on an epoch boundary, saves the whole state.
	if helpers.IsEpochStart(state.Slot()) {
		if err := s.beaconDB.SaveState(ctx, state, blockRoot); err != nil {
			return err
		}
		log.WithFields(logrus.Fields{
			"slot":      state.Slot(),
			"blockRoot": hex.EncodeToString(bytesutil.Trunc(blockRoot[:]))}).Info("Saved full state on epoch boundary")
		hotStateSaved.Inc()
	}

	// On an intermediate slots, save the hot state summary.
	epochRoot, err := s.loadEpochBoundaryRoot(ctx, blockRoot, state)
	if err != nil {
		return errors.Wrap(err, "could not get epoch boundary root to save hot state")
	}
	if err := s.beaconDB.SaveStateSummary(ctx, &pb.StateSummary{
		Slot:         state.Slot(),
		Root:         blockRoot[:],
		BoundaryRoot: epochRoot[:],
	}); err != nil {
		return err
	}
	stateSummarySaved.Inc()

	// Store the state in the cache.
	// Don't need to copy state given the state is not returned.
	s.hotStateCache.Put(blockRoot, state)

	return nil
}

// This loads a post finalized beacon state from the hot section of the DB. If necessary it will
// replay blocks from the nearest epoch boundary.
func (s *State) loadHotStateByRoot(ctx context.Context, blockRoot [32]byte) (*state.BeaconState, error) {
	ctx, span := trace.StartSpan(ctx, "stateGen.loadHotStateByRoot")
	defer span.End()

	// Load the cache.
	cachedState := s.hotStateCache.Get(blockRoot)
	if cachedState != nil {
		return cachedState, nil
	}

	summary, err := s.beaconDB.StateSummary(ctx, blockRoot)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, errUnknownHotSummary
	}
	targetSlot := summary.Slot

	boundaryState, err := s.beaconDB.State(ctx, bytesutil.ToBytes32(summary.BoundaryRoot))
	if err != nil {
		return nil, err
	}
	if boundaryState == nil {
		return nil, errUnknownBoundaryState
	}

	// Don't need to replay the blocks if we're already on an epoch boundary meaning target slot
	// is the same as the state slot.
	var hotState *state.BeaconState
	if targetSlot == boundaryState.Slot() {
		hotState = boundaryState
	} else {
		blks, err := s.LoadBlocks(ctx, boundaryState.Slot()+1, targetSlot, bytesutil.ToBytes32(summary.Root))
		if err != nil {
			return nil, errors.Wrap(err, "could not load blocks for hot state using root")
		}
		hotState, err = s.ReplayBlocks(ctx, boundaryState, blks, targetSlot)
		if err != nil {
			return nil, errors.Wrap(err, "could not replay blocks for hot state using root")
		}
	}

	// Save the cache in cache and copy the state since it is also returned in the end.
	s.hotStateCache.Put(blockRoot, hotState.Copy())

	return hotState, nil
}

// This loads a hot state by slot only where the slot lies between the epoch boundary points.
// This is a slower implementation given slot is the only argument. It require fetching
// all the blocks between the epoch boundary points.
func (s *State) loadHotIntermediateStateWithSlot(ctx context.Context, slot uint64) (*state.BeaconState, error) {
	ctx, span := trace.StartSpan(ctx, "stateGen.loadHotIntermediateStateWithSlot")
	defer span.End()

	// Gather epoch boundary information, this is where node starts to replay the blocks.
	boundarySlot := helpers.StartSlot(helpers.SlotToEpoch(slot))
	boundaryRoot, ok := s.epochBoundaryRoot(boundarySlot)
	if !ok {
		return nil, errUnknownBoundaryRoot
	}

	boundaryState, err := s.beaconDB.State(ctx, boundaryRoot)
	if err != nil {
		return nil, err
	}
	if boundaryState == nil {
		return nil, errUnknownBoundaryState
	}

	// Gather the last physical block root and the slot number.
	lastValidRoot, lastValidSlot, err := s.getLastValidBlock(ctx, slot)
	if err != nil {
		return nil, errors.Wrap(err, "could not get last valid block for hot state using slot")
	}

	// Load and replay blocks to get the intermediate state.
	replayBlks, err := s.LoadBlocks(ctx, boundaryState.Slot()+1, lastValidSlot, lastValidRoot)
	if err != nil {
		return nil, err
	}

	return s.ReplayBlocks(ctx, boundaryState, replayBlks, slot)
}

// This loads the epoch boundary root of a given state based on the state slot.
// If the epoch boundary does not have a valid block, it goes back to find the last
// slot which has a valid block.
func (s *State) loadEpochBoundaryRoot(ctx context.Context, blockRoot [32]byte, state *state.BeaconState) ([32]byte, error) {
	ctx, span := trace.StartSpan(ctx, "stateGen.loadEpochBoundaryRoot")
	defer span.End()

	boundarySlot := helpers.CurrentEpoch(state) * params.BeaconConfig().SlotsPerEpoch

	// Node first checks if epoch boundary root already exists in cache.
	r, ok := s.epochBoundarySlotToRoot[boundarySlot]
	if ok {
		return r, nil
	}

	// At epoch boundary, the root is just itself.
	if state.Slot() == boundarySlot {
		return blockRoot, nil
	}

	// Node uses genesis getters if the epoch boundary slot is on genesis slot.
	if boundarySlot == 0 {
		b, err := s.beaconDB.GenesisBlock(ctx)
		if err != nil {
			return [32]byte{}, err
		}

		r, err = ssz.HashTreeRoot(b.Block)
		if err != nil {
			return [32]byte{}, err
		}

		s.setEpochBoundaryRoot(boundarySlot, r)

		return r, nil
	}

	// Now to find the epoch boundary root via DB.
	filter := filters.NewFilter().SetStartSlot(boundarySlot).SetEndSlot(boundarySlot)
	rs, err := s.beaconDB.BlockRoots(ctx, filter)
	if err != nil {
		return [32]byte{}, err
	}
	// If the epoch boundary is a skip slot, traverse back to find the last valid state.
	if len(rs) == 0 {
		r, err = s.handleLastValidState(ctx, boundarySlot)
		if err != nil {
			return [32]byte{}, errors.Wrap(err, "could not get last valid root for epoch boundary by root")
		}
	} else if len(rs) == 1 {
		r = rs[0]
	} else {
		// This should not happen, there shouldn't be more than 1 epoch boundary root,
		// but adding this check to be save.
		return [32]byte{}, errors.New("incorrect length for epoch boundary root")
	}

	// Set the epoch boundary root cache.
	s.setEpochBoundaryRoot(boundarySlot, r)

	return r, nil
}

// This finds the last valid state from searching backwards starting at input slot
// and returns the root of the block which is used to process the state.
func (s *State) handleLastValidState(ctx context.Context, targetSlot uint64) ([32]byte, error) {
	ctx, span := trace.StartSpan(ctx, "stateGen.handleLastValidState")
	defer span.End()

	lastBlockRoot, _, err := s.getLastValidBlock(ctx, targetSlot)
	if err != nil {
		return [32]byte{}, errors.Wrap(err, "could not get last valid block for last valid state")
	}

	lastValidState, err := s.ComputeStateUpToSlot(ctx, targetSlot)
	if err != nil {
		return [32]byte{}, errors.Wrap(err, "could not compute state for last valid state")
	}

	// Only save the state if there's non with the last block root.
	if !s.beaconDB.HasState(ctx, lastBlockRoot) {
		if err := s.beaconDB.SaveState(ctx, lastValidState, lastBlockRoot); err != nil {
			return [32]byte{}, err
		}
	}

	return lastBlockRoot, nil
}
