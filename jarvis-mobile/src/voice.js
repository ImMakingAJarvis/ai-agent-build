// Voice input for the chat screen: tap-to-talk mic with silence auto-stop.
//
// Flow:  idle -> (tap) -> recording (VAD ticks every 100 ms) ->
//        (tap / 5 s of silence / 30 s cap) -> sending (POST /voice,
//        multipart upload; the server transcodes whatever container we
//        record in — see ensure_wav() in scripts/voice_service.py) ->
//        playing (TTS reply) -> idle
//
// The VAD decision logic lives in ./vad.js (ported from the PWA fix, PR
// #79) and is unit-tested in test/vad.test.js. This file only wires the
// recorder's metering level into it and does the upload + playback.

import { useCallback, useEffect, useRef, useState } from 'react';
import { Platform } from 'react-native';
import {
  RecordingPresets,
  createAudioPlayer,
  requestRecordingPermissionsAsync,
  setAudioModeAsync,
  useAudioRecorder,
} from 'expo-audio';
import { File, Paths } from 'expo-file-system';
import { headers, url } from './api';
import { VAD_TICK_MS, createVad } from './vad';

// HIGH_QUALITY preset: m4a/AAC on iOS and Android, webm on web — all
// decodable by the server's ffmpeg transcode (PR #78). Metering is what
// feeds the VAD; voice_communication adds echo cancellation + AGC on
// Android, which suits a hands-free home mic.
const RECORDER_OPTIONS = {
  ...RecordingPresets.HIGH_QUALITY,
  isMeteringEnabled: true,
  android: {
    ...RecordingPresets.HIGH_QUALITY.android,
    audioSource: 'voice_communication',
  },
};

// Attach the recorded file to a multipart form, per platform: native RN
// Blobs wrap a file uri; web must fetch the blob: URL first.
async function attachAudio(fd, uri) {
  const mime = Platform.OS === 'web' ? 'audio/webm' : 'audio/mp4';
  const name = Platform.OS === 'web' ? 'voice.webm' : 'voice.m4a';
  if (Platform.OS === 'web') {
    const blob = await (await fetch(uri)).blob();
    fd.append('file', blob, name);
  } else {
    fd.append('file', new Blob({ uri, type: mime }), name);
  }
}

// POST the recording to /voice. Returns {transcript, reply, audio} where
// audio is the TTS bytes (edge-tts emits MP3; the server labels the
// response audio/wav — the bytes are MP3 either way).
async function uploadVoice(settings, uri) {
  const fd = new FormData();
  await attachAudio(fd, uri);
  if (settings.model) fd.append('model', settings.model);
  const res = await fetch(url(settings, '/voice'), {
    method: 'POST',
    headers: headers(settings),
    body: fd,
  });
  if (!res.ok) {
    let msg = 'Voice request failed (HTTP ' + res.status + ')';
    try {
      const body = await res.json();
      if (body && body.error) msg = body.error;
    } catch (e) { /* not JSON */ }
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return {
    transcript: res.headers.get('X-Transcript') || '',
    reply: res.headers.get('X-Reply') || '',
    audio: await res.arrayBuffer(),
  };
}

// Play TTS bytes. Native: write to the app cache and play the file. Web:
// play a blob URL directly. Resolves when playback finishes (or errors —
// the text reply is already on screen by then, so a TTS failure is silent).
function playTts(audio, onDone) {
  let settled = false;
  const finish = () => {
    if (!settled) {
      settled = true;
      onDone();
    }
  };
  if (Platform.OS === 'web') {
    try {
      const u = URL.createObjectURL(new Blob([audio], { type: 'audio/mpeg' }));
      const a = new Audio(u);
      a.onended = () => { URL.revokeObjectURL(u); finish(); };
      a.onerror = () => { URL.revokeObjectURL(u); finish(); };
      a.play().catch(finish);
    } catch (e) {
      finish();
    }
    return null;
  }
  const file = new File(Paths.cache, 'tts-' + Date.now() + '.mp3');
  (async () => {
    let player = null;
    try {
      await file.write(new Uint8Array(audio));
      player = createAudioPlayer(file.uri);
      player.addListener('playbackStatusUpdate', (s) => {
        if (s.didJustFinish || s.error) {
          try { player.remove(); } catch (e) {}
          try { file.delete(); } catch (e) {}
          finish();
        }
      });
      player.play();
    } catch (e) {
      if (player) { try { player.remove(); } catch (e2) {} }
      finish();
    }
  })();
  return {
    // Lets a new turn (or unmount) cut off an in-flight reply.
    stop() {
      finish();
    },
  };
}

// phase: 'idle' | 'recording' | 'sending' | 'playing'
export function useVoice(settings, { onTurn, onTurnError }) {
  const [phase, setPhase] = useState('idle');
  const [seconds, setSeconds] = useState(0);

  const recorder = useAudioRecorder(RECORDER_OPTIONS);
  const refs = useRef({
    recorder,
    vad: null,
    timer: null,
    tts: null,
    settings,
    onTurn,
    onTurnError,
  });
  refs.current.recorder = recorder;
  refs.current.settings = settings;
  refs.current.onTurn = onTurn;
  refs.current.onTurnError = onTurnError;

  const resetUi = useCallback(() => {
    const r = refs.current;
    if (r.timer) { clearInterval(r.timer); r.timer = null; }
    if (r.vad) r.vad.stop();
    if (r.tts) { r.tts.stop(); r.tts = null; }
    setPhase('idle');
    setSeconds(0);
  }, []);

  const fail = useCallback(
    (msg) => {
      resetUi();
      if (refs.current.onTurnError) refs.current.onTurnError(msg);
    },
    [resetUi],
  );

  // Stop the recording for any reason (tap, VAD auto-stop, safety cap),
  // then send what was captured and play the reply.
  const stopAndSend = useCallback(async () => {
    const r = refs.current;
    if (!r.vad || !r.vad.isRunning) return; // already stopping / not recording
    r.vad.stop();
    if (r.timer) { clearInterval(r.timer); r.timer = null; }
    setPhase('sending');
    let uri = null;
    try {
      await r.recorder.stop();
      uri = r.recorder.uri;
    } catch (e) {
      fail('Could not finish the recording: ' + ((e && e.message) || e));
      return;
    }
    if (!uri) {
      fail('No audio was captured');
      return;
    }
    let result;
    try {
      result = await uploadVoice(r.settings, uri);
    } catch (e) {
      fail(String((e && e.message) || e));
      return;
    }
    if (r.onTurn) r.onTurn(result);
    setPhase('playing');
    r.tts = playTts(result.audio, () => {
      if (refs.current.tts === r.tts) refs.current.tts = null;
      setPhase('idle');
    });
  }, [fail, resetUi]);

  const start = useCallback(async () => {
    const r = refs.current;
    if (r.vad && r.vad.isRunning) return; // already recording
    try {
      const perm = await requestRecordingPermissionsAsync();
      if (!perm.granted) {
        fail('Microphone permission denied');
        return;
      }
      // playAndRecord so a reply can be spoken while the mic is available;
      // shouldRouteThroughEarpiece stays false, so TTS comes out of the
      // speaker on iOS.
      await setAudioModeAsync({
        playsInSilentMode: true,
        allowsRecording: true,
      });
      await r.recorder.prepareToRecordAsync();
      const t0 = Date.now();
      const vad = createVad();
      vad.start(t0);
      r.vad = vad;
      r.recorder.record();
      setPhase('recording');
      setSeconds(0);
      r.timer = setInterval(() => {
        let level;
        try {
          level = r.recorder.getStatus().metering;
        } catch (e) {
          level = undefined;
        }
        const now = Date.now();
        setSeconds(Math.floor((now - t0) / 1000));
        if (vad.tick(now, level) === 'stop' || vad.tick(now, level) === 'cap') {
          stopAndSend();
        }
      }, VAD_TICK_MS);
    } catch (e) {
      fail('Could not start the microphone: ' + ((e && e.message) || e));
    }
  }, [fail, stopAndSend]);

  const stop = useCallback(() => {
    if (refs.current.vad && refs.current.vad.isRunning) stopAndSend();
  }, [stopAndSend]);

  // Unmount: stop everything that is running.
  useEffect(
    () => () => {
      const r = refs.current;
      if (r.timer) clearInterval(r.timer);
      if (r.tts) r.tts.stop();
      if (r.recorder && r.recorder.isRecording) {
        try { r.recorder.stop(); } catch (e) {}
      }
    },
    [],
  );

  return { phase, seconds, start, stop };
}
