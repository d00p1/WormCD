package reconcile

import (
	"context"
	"sync"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
)

// fakeRuntime keeps the observed state of each target in memory. A successful
// Apply makes the observed state equal the planned desired state, the same
// result a converged cluster has. A failed Apply changes nothing.
type fakeRuntime struct {
	mu       sync.Mutex
	observed map[string]runtime.Observed // key: target name
	healthy  bool
	problems []string

	applyErr   error
	applyPanic any
	healthErr  error
	inspectErr error

	// gates blocks Apply for a target until the channel is closed or the context
	// ends. entered receives the target name when Apply starts blocking.
	gates   map[string]chan struct{}
	entered chan string

	inspects int
	applies  int
	prunes   int
	healths  int
	// inFlight counts targets between Inspect and Health; maxInFlight is its
	// peak per target. Serialized syncs of one target never overlap.
	inFlight    map[string]int
	maxInFlight map[string]int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		observed:    make(map[string]runtime.Observed),
		healthy:     true,
		gates:       make(map[string]chan struct{}),
		inFlight:    make(map[string]int),
		maxInFlight: make(map[string]int),
	}
}

func (f *fakeRuntime) Kind() runtime.Kind { return runtime.KindSwarm }

func (f *fakeRuntime) Inspect(_ context.Context, scope runtime.Scope) (runtime.Observed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	if f.inspectErr != nil {
		return runtime.Observed{}, f.inspectErr
	}
	f.inFlight[scope.Target]++
	if f.inFlight[scope.Target] > f.maxInFlight[scope.Target] {
		f.maxInFlight[scope.Target] = f.inFlight[scope.Target]
	}
	obs := f.observed[scope.Target]
	obs.Application = scope.Application
	obs.Target = scope.Target
	obs.Namespace = desired.TargetNamespace(scope.Application, scope.Target)
	return obs, nil
}

func (f *fakeRuntime) Apply(ctx context.Context, plan runtime.Plan) (runtime.Result, error) {
	f.mu.Lock()
	f.applies++
	gate := f.gates[plan.Target]
	panicValue := f.applyPanic
	f.mu.Unlock()

	if panicValue != nil {
		panic(panicValue)
	}

	if gate != nil {
		f.entered <- plan.Target
		select {
		case <-gate:
		case <-ctx.Done():
			return runtime.Result{}, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return runtime.Result{}, f.applyErr
	}
	f.observed[plan.Target] = observedOf(plan.Desired)
	return runtime.Result{}, nil
}

func (f *fakeRuntime) Prune(_ context.Context, _ runtime.Scope, _ runtime.Set) (runtime.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes++
	return runtime.PruneResult{}, nil
}

func (f *fakeRuntime) Health(_ context.Context, scope runtime.Scope) (runtime.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healths++
	f.inFlight[scope.Target]--
	if f.healthErr != nil {
		return runtime.Health{}, f.healthErr
	}
	return runtime.Health{Healthy: f.healthy, Problems: f.problems}, nil
}

// setImage changes the observed image of a service, as a manual change in the
// cluster would.
func (f *fakeRuntime) setImage(target, service, image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	services := f.observed[target].Services
	for i := range services {
		if services[i].Name == service {
			services[i].Image = image
		}
	}
}

// serviceName returns the only observed service of the target.
func (f *fakeRuntime) serviceName(target string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observed[target].Services[0].Name
}

func (f *fakeRuntime) calls() (inspects, applies, prunes, healths int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspects, f.applies, f.prunes, f.healths
}

// images maps each observed service of the target to its image.
func (f *fakeRuntime) images(target string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string)
	for _, s := range f.observed[target].Services {
		out[s.Name] = s.Image
	}
	return out
}

// observedOf copies the managed objects of a desired state into an observed
// state. Copies keep later edits of the fake from changing the desired state.
func observedOf(state desired.Desired) runtime.Observed {
	obs := runtime.Observed{
		Services: append([]desired.Service(nil), state.Services...),
		Networks: append([]desired.Network(nil), state.Networks...),
		Configs:  append([]desired.Config(nil), state.Configs...),
		Volumes:  append([]desired.Volume(nil), state.Volumes...),
	}
	return obs
}
