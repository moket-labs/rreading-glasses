package internal

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type denormCompletion struct {
	nopersist
	done chan struct{}
}

func (p *denormCompletion) Delete(context.Context, int64) error {
	close(p.done)
	return nil
}

func TestDenormQueueMetrics(t *testing.T) {
	getter := NewMockgetter(gomock.NewController(t))
	p := &denormCompletion{done: make(chan struct{})}
	ctrl, err := NewController(newMemoryCache(), getter, p, nil)
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	getter.EXPECT().GetWork(gomock.Any(), int64(1), nil).DoAndReturn(
		func(context.Context, int64, editionsCallback) ([]byte, int64, error) {
			close(entered)
			<-release
			return nil, 0, errNotFound
		})
	getter.EXPECT().GetWork(gomock.Any(), int64(2), nil).Return(nil, int64(0), errNotFound)

	runDone := make(chan struct{})
	go func() {
		ctrl.Run(t.Context())
		close(runDone)
	}()
	defer func() {
		close(release)
		// The FIFO completion marker proves the queued work was processed
		// before closing inputs; closing a nonempty accumulator drops work.
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("denormalization did not drain")
		}
		close(ctrl.refreshC)
		close(ctrl.denormC)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("controller did not exit")
		}
	}()

	ctrl.denormC <- edge{kind: workEdge, parentID: 1, childIDs: newSet(int64(10))}
	<-entered
	ctrl.denormC <- edge{kind: workEdge, parentID: 2, childIDs: newSet(int64(20))}
	// The next accepted send proves the previous push finished. No sleeps
	// or scheduler assumptions: the worker is still blocked on release.
	ctrl.denormC <- edge{kind: refreshDone}
	assert.Equal(t, float64(1), ctrl.metrics.denormWaitingGet(), "queued children must be reported while the worker is busy")
}

// Hold the accumulator exactly between its send and pop, allowing the worker
// to finish first: the schedule that used to leave waitForDenorm stuck forever.
type delayedEdgePop struct {
	*edgebuf
	release chan struct{}
	popped  chan struct{}
}

func (b *delayedEdgePop) pop() edge {
	<-b.release
	e := b.edgebuf.pop()
	close(b.popped)
	return e
}

func TestDenormQueueMetricsAfterHandoff(t *testing.T) {
	metrics := newControllerMetrics(nil)
	b := &delayedEdgePop{
		edgebuf: &edgebuf{sizeChanged: metrics.denormWaitingSet},
		release: make(chan struct{}),
		popped:  make(chan struct{}),
	}
	b.push(edge{kind: workEdge, parentID: 1, childIDs: newSet(int64(10))})
	producer := make(chan edge)
	consumer := accumulate(producer, b)
	defer func() {
		close(producer)
		// Join the accumulator instead of leaving a blocked goroutine.
		_, ok := <-consumer
		assert.False(t, ok)
	}()

	e := <-consumer
	assert.Equal(t, int64(1), e.parentID)
	// This is the old worker-side sample. The handed-off edge has not
	// been popped yet, so it reports one despite completed processing.
	assert.Equal(t, float64(1), metrics.denormWaitingGet())
	close(b.release)
	<-b.popped
	assert.Zero(t, b.len())
	assert.Zero(t, metrics.denormWaitingGet(), "the final pop must clear the gauge without another worker iteration")
}
