package ruler

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/dskit/kv/consul"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/ring/testutils"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/testutil"
)

// TestRulerShutdown tests shutting down ruler unregisters correctly
func TestRulerShutdown(t *testing.T) {
	ctx := context.Background()

	config, cleanup := defaultRulerConfig(t, newMockRuleStore(mockRules))
	defer cleanup()

	r, rcleanup := newRuler(t, config)
	defer rcleanup()

	r.cfg.EnableSharding = true
	ringStore, closer := consul.NewInMemoryClient(ring.GetCodec(), log.NewNopLogger())
	t.Cleanup(func() { assert.NoError(t, closer.Close()) })

	err := enableSharding(r, ringStore)
	require.NoError(t, err)

	require.NoError(t, services.StartAndAwaitRunning(ctx, r))
	defer services.StopAndAwaitTerminated(ctx, r) //nolint:errcheck

	// Wait until the tokens are registered in the ring
	testutil.Poll(t, 100*time.Millisecond, config.Ring.NumTokens, func() interface{} {
		return testutils.NumTokens(ringStore, "localhost", ring.RulerRingKey)
	})

	require.Equal(t, ring.ACTIVE, r.lifecycler.GetState())

	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), r))

	// Wait until the tokens are unregistered from the ring
	testutil.Poll(t, 100*time.Millisecond, 0, func() interface{} {
		return testutils.NumTokens(ringStore, "localhost", ring.RulerRingKey)
	})
}

func TestRuler_RingLifecyclerShouldAutoForgetUnhealthyInstances(t *testing.T) {
	const unhealthyInstanceID = "unhealthy-id"
	const heartbeatTimeout = time.Minute

	ctx := context.Background()
	config, cleanup := defaultRulerConfig(t, newMockRuleStore(mockRules))
	defer cleanup()
	r, rcleanup := newRuler(t, config)
	defer rcleanup()
	r.cfg.EnableSharding = true
	r.cfg.Ring.HeartbeatPeriod = 100 * time.Millisecond
	r.cfg.Ring.HeartbeatTimeout = heartbeatTimeout

	ringStore, closer := consul.NewInMemoryClient(ring.GetCodec(), log.NewNopLogger())
	t.Cleanup(func() { assert.NoError(t, closer.Close()) })

	err := enableSharding(r, ringStore)
	require.NoError(t, err)

	require.NoError(t, services.StartAndAwaitRunning(ctx, r))
	defer services.StopAndAwaitTerminated(ctx, r) //nolint:errcheck

	// Add an unhealthy instance to the ring.
	require.NoError(t, ringStore.CAS(ctx, ring.RulerRingKey, func(in interface{}) (interface{}, bool, error) {
		ringDesc := ring.GetOrCreateRingDesc(in)

		instance := ringDesc.AddIngester(unhealthyInstanceID, "1.1.1.1", "", generateSortedTokens(config.Ring.NumTokens), ring.ACTIVE, time.Now())
		instance.Timestamp = time.Now().Add(-(ringAutoForgetUnhealthyPeriods + 1) * heartbeatTimeout).Unix()
		ringDesc.Ingesters[unhealthyInstanceID] = instance

		return ringDesc, true, nil
	}))

	// Ensure the unhealthy instance is removed from the ring.
	testutil.Poll(t, time.Second*5, false, func() interface{} {
		d, err := ringStore.Get(ctx, ring.RulerRingKey)
		if err != nil {
			return err
		}

		_, ok := ring.GetOrCreateRingDesc(d).Ingesters[unhealthyInstanceID]
		return ok
	})
}

func generateSortedTokens(numTokens int) ring.Tokens {
	tokens := ring.GenerateTokens(numTokens, nil)

	// Ensure generated tokens are sorted.
	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i] < tokens[j]
	})

	return ring.Tokens(tokens)
}