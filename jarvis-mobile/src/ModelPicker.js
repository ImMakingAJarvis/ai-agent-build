import React, { useCallback, useEffect, useState } from 'react';
import { Modal, Pressable, StyleSheet, Text, View } from 'react-native';
import { fetchModels } from './api';

// Brain model picker (GET /models). The list grows when more models are
// loaded on the engine — no app update needed.
export default function ModelPicker({ cfg, model, onSelect }) {
  const [models, setModels] = useState(null); // null = still loading
  const [error, setError] = useState('');
  const [open, setOpen] = useState(false);

  const refresh = useCallback(() => {
    fetchModels(cfg)
      .then((d) => {
        setModels(d.models || []);
        setError('');
      })
      .catch((e) => {
        setModels([]);
        setError(e.message || String(e));
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cfg.serverUrl, cfg.authUser, cfg.authPass]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const current =
    (models || []).find((m) => m.id === model) ||
    (models || []).find((m) => m.default) ||
    null;

  return (
    <>
      <Pressable style={styles.chip} onPress={() => setOpen(true)} hitSlop={6}>
        <Text style={styles.chipText} numberOfLines={1}>
          {'🧠 ' + (models === null ? '…' : current ? current.name : 'no models') + ' ▾'}
        </Text>
      </Pressable>
      <Modal transparent visible={open} animationType="fade" onRequestClose={() => setOpen(false)}>
        <Pressable style={styles.backdrop} onPress={() => setOpen(false)}>
          <View style={styles.sheet} onStartShouldSetResponder={() => true}>
            <Text style={styles.title}>Model</Text>
            {error ? <Text style={styles.err}>{'⚠ ' + error}</Text> : null}
            {(models || []).map((m) => (
              <Pressable
                key={m.id}
                style={styles.row}
                onPress={() => {
                  onSelect(m.id);
                  setOpen(false);
                }}
              >
                <Text style={[styles.rowText, m.id === model && styles.rowSel]}>
                  {(m.id === model ? '✓ ' : '') + m.name + (m.default ? ' (default)' : '')}
                </Text>
              </Pressable>
            ))}
            <Pressable style={styles.row} onPress={refresh}>
              <Text style={styles.rowText}>{'↻ Refresh'}</Text>
            </Pressable>
          </View>
        </Pressable>
      </Modal>
    </>
  );
}

const styles = StyleSheet.create({
  chip: {
    backgroundColor: '#171f2a',
    borderColor: '#223041',
    borderWidth: 1,
    borderRadius: 14,
    paddingHorizontal: 10,
    paddingVertical: 4,
    maxWidth: 180,
  },
  chipText: {
    color: '#8fc6ff',
    fontSize: 13,
  },
  backdrop: {
    flex: 1,
    backgroundColor: 'rgba(0,0,0,0.6)',
    justifyContent: 'center',
    padding: 24,
  },
  sheet: {
    backgroundColor: '#171f2a',
    borderRadius: 12,
    borderWidth: 1,
    borderColor: '#223041',
    padding: 12,
    maxHeight: '70%',
  },
  title: {
    color: '#e6edf3',
    fontSize: 16,
    fontWeight: '600',
    marginBottom: 8,
  },
  err: {
    color: '#f85149',
    fontSize: 13,
    marginBottom: 8,
  },
  row: {
    paddingVertical: 10,
    paddingHorizontal: 8,
    borderBottomWidth: 1,
    borderBottomColor: '#223041',
  },
  rowText: {
    color: '#e6edf3',
    fontSize: 14,
  },
  rowSel: {
    color: '#3fb6ff',
  },
});
