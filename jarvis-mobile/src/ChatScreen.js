import React, { useCallback, useRef, useState } from 'react';
import {
  FlatList,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import { sendChat } from './api';
import { useVoice } from './voice';
import ModelPicker from './ModelPicker';

// "Queue that as a job" — the fixed prompt the PWA sends; the brain's
// schedule_job tool then actually queues the job.
const QUEUE_PROMPT =
  'Please turn that into a job: lay out the plan, then queue it with the schedule_job tool.';

let seq = 0;
const nid = () => Date.now() + '-' + seq++;

function short(s) {
  s = String(s || '');
  return s.length > 80 ? s.slice(0, 77) + '…' : s;
}

// One chat row. m: {id, role, content, actions?, error?} or {busy:true}.
function Message({ m }) {
  if (m.busy) {
    return (
      <View style={[styles.row, styles.rowBot]}>
        <View style={[styles.bubble, styles.bubbleBot]}>
          <Text style={{ color: '#8b98a9' }}>thinking…</Text>
        </View>
      </View>
    );
  }
  const isUser = m.role === 'user';
  return (
    <View style={[styles.row, isUser ? styles.rowUser : styles.rowBot]}>
      <View
        style={[
          styles.bubble,
          isUser ? styles.bubbleUser : m.error ? styles.bubbleErr : styles.bubbleBot,
        ]}
      >
        <Text style={styles.msgText}>{m.content}</Text>
        {m.actions && m.actions.length > 0 ? (
          <View style={styles.chips}>
            {m.actions.map((a, i) => (
              <View
                key={i}
                style={[styles.chip, a.scheduled ? styles.chipQueued : styles.chipSearch]}
              >
                <Text
                  style={[styles.chipText, a.scheduled ? styles.chipQueuedText : styles.chipSearchText]}
                  numberOfLines={1}
                >
                  {(a.scheduled ? '⚙ Queued: ' : '🔎 ') + short(a.result)}
                </Text>
              </View>
            ))}
          </View>
        ) : null}
      </View>
    </View>
  );
}

export default function ChatScreen({ settings, onOpenSettings, onModelSelect }) {
  const [messages, setMessages] = useState([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const listRef = useRef(null);

  const push = useCallback((m) => {
    setMessages((prev) => [...prev, m]);
  }, []);

  // A finished voice turn lands in the same history as typed messages, so
  // later /chat calls keep the transcript + spoken reply in context.
  const voiceOnTurn = useCallback(
    ({ transcript, reply }) => {
      if (transcript) push({ id: nid(), role: 'user', content: transcript });
      push({ id: nid(), role: 'assistant', content: reply || '' });
    },
    [push],
  );
  const voiceOnError = useCallback(
    (msg) => {
      push({ id: nid(), role: 'assistant', content: '🎙 ' + msg, error: true });
    },
    [push],
  );
  const voice = useVoice(settings, { onTurn: voiceOnTurn, onTurnError: voiceOnError });

  const doSend = useCallback(
    async (text) => {
      const content = text.trim();
      if (!content || busy) return;
      setInput('');
      // The API is stateless and the client owns the conversation: send the
      // full history, minus error bubbles (those are UI-only).
      const history = messages
        .filter((m) => !m.error && !m.busy)
        .map((m) => ({ role: m.role, content: m.content }));
      history.push({ role: 'user', content });
      push({ id: nid(), role: 'user', content });
      setBusy(true);
      try {
        const res = await sendChat(settings, history, settings.model);
        push({
          id: nid(),
          role: 'assistant',
          content: res.reply || '',
          actions: res.actions || [],
        });
      } catch (e) {
        push({ id: nid(), role: 'assistant', content: String((e && e.message) || e), error: true });
      } finally {
        setBusy(false);
      }
    },
    [busy, messages, settings, push],
  );

  const last = messages[messages.length - 1];
  const canQueue =
    !busy && last && last.role === 'assistant' && !last.error;

  const data = busy ? [...messages, { id: '__busy__', busy: true }] : messages;

  return (
    <View style={styles.flex}>
      <View style={styles.header}>
        <Text style={styles.title}>Jarvis</Text>
        <ModelPicker cfg={settings} model={settings.model} onSelect={onModelSelect} />
        <Pressable style={styles.gear} onPress={onOpenSettings} hitSlop={8}>
          <Text style={{ color: '#8b98a9', fontSize: 18 }}>⚙</Text>
        </Pressable>
      </View>

      <FlatList
        ref={listRef}
        style={styles.flex}
        data={data}
        keyExtractor={(item) => item.id}
        renderItem={({ item }) => <Message m={item} />}
        contentContainerStyle={data.length === 0 ? styles.empty : null}
        onContentSizeChange={() => {
          if (listRef.current) listRef.current.scrollToEnd({ animated: false });
        }}
      />

      <KeyboardAvoidingView behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
        {canQueue ? (
          <Pressable style={styles.queueBtn} onPress={() => doSend(QUEUE_PROMPT)} hitSlop={4}>
            <Text style={{ color: '#7ee2a0', fontSize: 13 }}>🧩 Queue that as a job</Text>
          </Pressable>
        ) : null}
        {voice.phase === 'recording' || voice.phase === 'sending' ? (
          <View style={styles.voiceStatus}>
            <Text style={styles.voiceStatusText}>
              {voice.phase === 'recording'
                ? '● Listening — auto-stops after silence, tap ⏹ to send now'
                : 'Listening…'}
            </Text>
          </View>
        ) : null}
        <View style={styles.inputBar}>
          <Pressable
            style={[
              styles.micBtn,
              voice.phase === 'recording'
                ? styles.micRec
                : (busy || voice.phase === 'sending' || voice.phase === 'playing') && styles.sendDisabled,
            ]}
            onPress={voice.phase === 'recording' ? voice.stop : voice.start}
            disabled={
              busy || voice.phase === 'sending' || voice.phase === 'playing'
            }
            hitSlop={4}
          >
            <Text style={{ color: '#0b0f14', fontSize: 16 }}>
              {voice.phase === 'recording'
                ? '⏹ ' + voice.seconds + 's'
                : voice.phase === 'sending'
                  ? '…'
                  : voice.phase === 'playing'
                    ? '🎧'
                    : '🎙'}
            </Text>
          </Pressable>
          <TextInput
            style={styles.input}
            value={input}
            onChangeText={setInput}
            placeholder="Message Jarvis…"
            placeholderTextColor="#5b6b7d"
            returnKeyType="send"
            onSubmitEditing={() => doSend(input)}
            blurOnSubmit={Platform.OS !== 'web'}
          />
          <Pressable
            style={[styles.sendBtn, (busy || !input.trim()) && styles.sendDisabled]}
            onPress={() => doSend(input)}
            hitSlop={4}
          >
            <Text style={{ color: '#0b0f14', fontSize: 16, fontWeight: '700' }}>➤</Text>
          </Pressable>
        </View>
      </KeyboardAvoidingView>
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 8,
    paddingHorizontal: 12,
    paddingVertical: 8,
    backgroundColor: '#121821',
    borderBottomWidth: 1,
    borderBottomColor: '#223041',
  },
  title: {
    color: '#e6edf3',
    fontSize: 16,
    fontWeight: '700',
    flex: 1,
  },
  gear: {
    padding: 4,
  },
  empty: {
    flexGrow: 1,
    justifyContent: 'center',
    alignItems: 'center',
    padding: 24,
  },
  row: {
    flexDirection: 'row',
    paddingVertical: 3,
    paddingHorizontal: 10,
  },
  rowUser: {
    justifyContent: 'flex-end',
  },
  rowBot: {
    justifyContent: 'flex-start',
  },
  bubble: {
    maxWidth: '85%',
    borderRadius: 14,
    paddingHorizontal: 12,
    paddingVertical: 8,
  },
  bubbleUser: {
    backgroundColor: '#1f6feb',
    borderTopRightRadius: 4,
  },
  bubbleBot: {
    backgroundColor: '#1c2733',
    borderTopLeftRadius: 4,
  },
  bubbleErr: {
    backgroundColor: '#3a1d1d',
    borderColor: '#7a3b3b',
    borderWidth: 1,
    borderTopLeftRadius: 4,
  },
  msgText: {
    color: '#e6edf3',
    fontSize: 15,
    lineHeight: 21,
  },
  chips: {
    marginTop: 6,
    gap: 4,
  },
  chip: {
    borderRadius: 8,
    paddingHorizontal: 8,
    paddingVertical: 4,
    maxWidth: '100%',
  },
  chipQueued: {
    backgroundColor: 'rgba(63,185,80,0.15)',
    borderColor: 'rgba(63,185,80,0.4)',
    borderWidth: 1,
  },
  chipSearch: {
    backgroundColor: 'rgba(63,182,255,0.10)',
    borderColor: 'rgba(63,182,255,0.3)',
    borderWidth: 1,
  },
  chipText: {
    fontSize: 12,
  },
  chipQueuedText: {
    color: '#7ee2a0',
  },
  chipSearchText: {
    color: '#8fc6ff',
  },
  queueBtn: {
    alignSelf: 'flex-end',
    marginHorizontal: 10,
    marginBottom: 4,
    backgroundColor: '#122117',
    borderColor: '#2d4a35',
    borderWidth: 1,
    borderRadius: 12,
    paddingHorizontal: 10,
    paddingVertical: 5,
  },
  inputBar: {
    flexDirection: 'row',
    alignItems: 'flex-end',
    gap: 8,
    paddingHorizontal: 10,
    paddingVertical: 8,
    backgroundColor: '#121821',
    borderTopWidth: 1,
    borderTopColor: '#223041',
  },
  input: {
    flex: 1,
    backgroundColor: '#171f2a',
    borderColor: '#223041',
    borderWidth: 1,
    borderRadius: 18,
    color: '#e6edf3',
    fontSize: 15,
    paddingHorizontal: 14,
    paddingVertical: 8,
    maxHeight: 96,
  },
  sendBtn: {
    backgroundColor: '#3fb6ff',
    borderRadius: 18,
    width: 38,
    height: 38,
    alignItems: 'center',
    justifyContent: 'center',
  },
  micBtn: {
    backgroundColor: '#3fb6ff',
    borderRadius: 18,
    minWidth: 38,
    height: 38,
    paddingHorizontal: 8,
    alignItems: 'center',
    justifyContent: 'center',
  },
  micRec: {
    backgroundColor: '#ff5f56',
  },
  voiceStatus: {
    paddingHorizontal: 14,
    paddingVertical: 4,
    backgroundColor: '#1a1214',
  },
  voiceStatusText: {
    color: '#ff8a80',
    fontSize: 12,
  },
  sendDisabled: {
    backgroundColor: '#223041',
  },
});
