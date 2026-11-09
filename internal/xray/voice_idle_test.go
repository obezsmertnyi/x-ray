package xray

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func withIdleTimeout(d time.Duration) func(*VoiceSession) {
	return func(s *VoiceSession) { s.idleTimeout = d }
}

func farewells(l *fakeLive) int {
	n := 0
	for _, m := range l.received() {
		if m == "content" {
			n++
		}
	}
	return n
}

// A session nobody speaks in ends: once the idle timeout passes, the model is
// asked to save the session memory, say goodbye and end the session, once.
func TestVoiceSession_IdleSessionGetsTheFarewellOnce(t *testing.T) {
	live := newFakeLive()
	s, _ := startResumeSession(t, live, &fakeConnector{}, true, withIdleTimeout(80*time.Millisecond))

	require.Eventually(t, func() bool { return farewells(live) == 1 }, 3*time.Second, 5*time.Millisecond)
	assert.True(t, s.idleFarewellSent.Load())
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, farewells(live), "the idle farewell is injected once")
}

// Speech keeps the session open; silent microphone frames do not.
func TestVoiceSession_SpeechKeepsTheSessionOpenSilenceDoesNot(t *testing.T) {
	voiced, quiet := []byte{0, 0x10, 0, 0x10}, make([]byte, 4)

	t.Run("speech", func(t *testing.T) {
		live := newFakeLive()
		_, browser := startResumeSession(t, live, &fakeConnector{}, false, withIdleTimeout(500*time.Millisecond))
		for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(30 * time.Millisecond) {
			require.NoError(t, browser.WriteJSON(VoiceBrowserMessage{Audio: voiced}))
		}
		assert.Zero(t, farewells(live), "no farewell while the user speaks")
		require.Eventually(t, func() bool { return farewells(live) == 1 }, 3*time.Second, 5*time.Millisecond, "the farewell once the speech stops")
	})
	t.Run("silent microphone frames", func(t *testing.T) {
		live := newFakeLive()
		_, browser := startResumeSession(t, live, &fakeConnector{}, false, withIdleTimeout(150*time.Millisecond))
		for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end) && farewells(live) == 0; time.Sleep(20 * time.Millisecond) {
			require.NoError(t, browser.WriteJSON(VoiceBrowserMessage{Audio: quiet}))
		}
		assert.Equal(t, 1, farewells(live), "a live microphone stream of silence is idle")
	})
}

// Model output counts as activity, like the user's speech.
func TestVoiceSession_ModelOutputKeepsTheSessionOpen(t *testing.T) {
	live := newFakeLive()
	_, browser := startResumeSession(t, live, &fakeConnector{}, false, withIdleTimeout(500*time.Millisecond))
	for i := range 30 { // 1.2 s of output, longer than the timeout
		live.push(transcript("answer part " + string(rune('A'+i))))
		readUntil(t, browser, hasTranscript("answer part "+string(rune('A'+i))))
		time.Sleep(40 * time.Millisecond)
	}
	assert.Zero(t, farewells(live), "no farewell while the model speaks")
}

// Without a timeout (the default) a session is never ended for idleness.
func TestVoiceSession_NoIdleTimeoutByDefault(t *testing.T) {
	live := newFakeLive()
	startResumeSession(t, live, &fakeConnector{}, true)
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, farewells(live))
}

func TestVoiceManager_IdleTimeoutCROverridesFlag(t *testing.T) {
	vm := newVoiceManagerWithConfig(&VoiceSessionConfig{})
	vm.SetIdleTimeout(5 * time.Minute)
	assert.Equal(t, 5*time.Minute, vm.effectiveIdleTimeout(), "flag default when the CR leaves it unset")

	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{IdleTimeoutSeconds: 180})
	vm.SetIdleTimeout(5 * time.Minute)
	assert.Equal(t, 3*time.Minute, vm.effectiveIdleTimeout(), "CR wins over the flag")

	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{})
	assert.Zero(t, vm.effectiveIdleTimeout(), "off unless set")

	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{IdleTimeoutSeconds: 5})
	assert.Equal(t, minIdleTimeout, vm.effectiveIdleTimeout(), "a timeout shorter than an answer's playback is raised")
	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{})
	vm.SetIdleTimeout(10 * time.Second)
	assert.Equal(t, minIdleTimeout, vm.effectiveIdleTimeout(), "the flag is raised too")
	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{IdleTimeoutSeconds: 1 << 40})
	assert.Equal(t, maxIdleTimeout, vm.effectiveIdleTimeout(), "a huge value is capped, not overflowed")
}

// Only one farewell: none for idleness once the session is ending or the
// daily-limit farewell went out.
func TestVoiceSession_NoIdleFarewellWhenTheSessionIsAlreadyEnding(t *testing.T) {
	for name, set := range map[string]func(*VoiceSession){
		"end_session":          func(s *VoiceSession) { s.pendingHangup.Store(true) },
		"daily-limit farewell": func(s *VoiceSession) { s.limitHangupSent.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			live := newFakeLive()
			startResumeSession(t, live, &fakeConnector{}, false, withIdleTimeout(50*time.Millisecond), set)
			time.Sleep(400 * time.Millisecond)
			assert.Zero(t, farewells(live))
		})
	}
}

// The grace timer of an idle farewell is stopped when the session ends first, so
// it does not log a timeout for a session that is gone.
func TestVoiceSession_FarewellGraceTimerStopsWithTheSession(t *testing.T) {
	live := newFakeLive()
	s, _ := startResumeSession(t, live, &fakeConnector{}, false, withIdleTimeout(50*time.Millisecond))
	require.Eventually(t, func() bool { return farewells(live) == 1 }, 3*time.Second, 5*time.Millisecond)
	s.Close()
	s.farewellMu.Lock()
	defer s.farewellMu.Unlock()
	require.Len(t, s.farewellTimers, 1)
	assert.False(t, s.farewellTimers[0].Stop(), "already stopped by the session's end")
}

func TestConfigWatcher_ParsesIdleTimeout(t *testing.T) {
	w := &ConfigWatcher{}
	cell := &mcell.MemoryCell{}
	cell.Name = SystemConfigCRName
	cell.Spec.Kind = "system_configuration"
	cell.Spec.VoiceConfig = &mcell.VoiceSessionConfig{IdleTimeoutSeconds: 180}
	w.update(cell)
	assert.Equal(t, int64(180), w.Get().IdleTimeoutSeconds)

	cell.Spec.VoiceConfig = &mcell.VoiceSessionConfig{}
	w.update(cell)
	assert.Zero(t, w.Get().IdleTimeoutSeconds, "unset in the CR so the flag applies")
}

// Context window compression (ADR-039 capability 3): off unless a trigger is
// set; the CR wins over the flags; a trigger outside 5,000-128,000 is moved in
// and a target that is not below it is left to the server.
func TestVoiceManager_ContextCompression(t *testing.T) {
	vm := newVoiceManagerWithConfig(&VoiceSessionConfig{})
	tr, tg := vm.effectiveContextCompression()
	assert.Zero(t, tr, "off by default")
	assert.Zero(t, tg)

	vm.SetContextCompression(32_000, 0)
	tr, tg = vm.effectiveContextCompression()
	assert.Equal(t, 32_000, tr, "the flag")
	assert.Zero(t, tg, "the server picks the target")

	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{ContextTriggerTokens: 64_000, ContextTargetTokens: 16_000})
	vm.SetContextCompression(32_000, 0)
	tr, tg = vm.effectiveContextCompression()
	assert.Equal(t, 64_000, tr, "the CR wins")
	assert.Equal(t, 16_000, tg)

	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{ContextTriggerTokens: 100, ContextTargetTokens: 200})
	tr, tg = vm.effectiveContextCompression()
	assert.Equal(t, minContextTriggerTokens, tr, "a trigger below the range is raised")
	assert.Equal(t, 200, tg, "a target below the trigger is kept")

	vm = newVoiceManagerWithConfig(&VoiceSessionConfig{ContextTriggerTokens: 100, ContextTargetTokens: 6_000})
	tr, tg = vm.effectiveContextCompression()
	assert.Equal(t, minContextTriggerTokens, tr)
	assert.Zero(t, tg, "a target not below the trigger is dropped")

	assert.True(t, ValidContextTrigger(0))
	assert.True(t, ValidContextTrigger(32_000))
	assert.False(t, ValidContextTrigger(4_999))
	assert.False(t, ValidContextTrigger(128_001))
}

func TestContextCompressionConfig(t *testing.T) {
	assert.Nil(t, contextCompression(0, 0), "off sends no compression config")
	cw := contextCompression(32_000, 0)
	require.NotNil(t, cw)
	require.NotNil(t, cw.TriggerTokens)
	assert.Equal(t, int64(32_000), *cw.TriggerTokens)
	require.NotNil(t, cw.SlidingWindow, "a sliding window, as ADR-039 decides")
	assert.Nil(t, cw.SlidingWindow.TargetTokens, "no target: the server uses half the trigger")
	cw = contextCompression(32_000, 16_000)
	require.NotNil(t, cw.SlidingWindow.TargetTokens)
	assert.Equal(t, int64(16_000), *cw.SlidingWindow.TargetTokens)
}

func TestConfigWatcher_ParsesContextCompression(t *testing.T) {
	w := &ConfigWatcher{}
	cell := &mcell.MemoryCell{}
	cell.Name = SystemConfigCRName
	cell.Spec.Kind = "system_configuration"
	cell.Spec.VoiceConfig = &mcell.VoiceSessionConfig{ContextTriggerTokens: 32_000, ContextTargetTokens: 16_000}
	w.update(cell)
	assert.Equal(t, 32_000, w.Get().ContextTriggerTokens)
	assert.Equal(t, 16_000, w.Get().ContextTargetTokens)
}
