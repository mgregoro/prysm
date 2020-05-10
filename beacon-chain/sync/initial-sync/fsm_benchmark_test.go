package initialsync

import (
	"testing"
)

func BenchmarkStateMachine_trigger(b *testing.B) {
	sm := newStateMachine()

	sm.addHandler(stateNew, eventSchedule, func(state *epochState, in interface{}) (id stateID, err error) {
		response, ok := in.(*fetchRequestParams)
		if !ok {
			return 0, errInputNotFetchRequestParams
		}
		_ = response.count
		return stateScheduled, nil
	})
	sm.addHandler(stateScheduled, eventDataReceived, func(state *epochState, in interface{}) (id stateID, err error) {
		response, ok := in.(*fetchRequestParams)
		if !ok {
			return 0, errInputNotFetchRequestParams
		}
		_ = response.count
		return stateDataParsed, nil
	})
	sm.addHandler(stateDataParsed, eventReadyToSend, func(state *epochState, in interface{}) (id stateID, err error) {
		response, ok := in.(*fetchRequestParams)
		if !ok {
			return 0, errInputNotFetchRequestParams
		}
		_ = response.count
		return stateSent, nil
	})
	sm.addHandler(stateSkipped, eventExtendWindow, func(state *epochState, in interface{}) (id stateID, err error) {
		response, ok := in.(*fetchRequestParams)
		if !ok {
			return 0, errInputNotFetchRequestParams
		}
		_ = response.count
		return stateNew, nil
	})
	sm.addHandler(stateSent, eventCheckStale, func(state *epochState, in interface{}) (id stateID, err error) {
		response, ok := in.(*fetchRequestParams)
		if !ok {
			return 0, errInputNotFetchRequestParams
		}
		_ = response.count
		return stateNew, nil
	})

	for i := uint64(0); i < lookaheadEpochs; i++ {
		sm.addEpochState(i)
	}

	b.ReportAllocs()
	b.ResetTimer()

	event, ok := sm.events[eventSchedule]
	if !ok {
		b.Errorf("event not found: %v", eventSchedule)
	}

	for i := 0; i < b.N; i++ {
		data := &fetchRequestParams{
			start: 23,
			count: 32,
		}
		err := sm.epochs[1].trigger(event, data)
		if err != nil {
			b.Fatal(err)
		}
	}
}
