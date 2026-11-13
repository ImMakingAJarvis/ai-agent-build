import React, { useState } from 'react';
import {
  Modal,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';

// Server settings: base URL + optional credentials. The service uses HTTP
// Basic (same as the PWA); with service auth off the fields are ignored, so
// leaving them blank works out of the box.
export default function SettingsSheet({ settings, onClose, onSave }) {
  const [serverUrl, setServerUrl] = useState(settings.serverUrl);
  const [authUser, setAuthUser] = useState(settings.authUser);
  const [authPass, setAuthPass] = useState(settings.authPass);

  const valid = /^https?:\/\/.+/.test(serverUrl.trim());

  return (
    <Modal transparent visible animationType="slide" onRequestClose={onClose}>
      <Pressable style={styles.backdrop} onPress={onClose}>
        <View style={styles.sheet} onStartShouldSetResponder={() => true}>
          <Text style={styles.title}>Settings</Text>

          <Text style={styles.label}>Server URL</Text>
          <TextInput
            style={styles.input}
            value={serverUrl}
            onChangeText={setServerUrl}
            autoCapitalize="none"
            autoCorrect={false}
            placeholder="https://111.1.2.27:8443"
            placeholderTextColor="#5b6b7d"
          />
          <Text style={styles.hint}>
            LAN/VPN only. On an Android emulator the host's plain-HTTP port is
            http://10.0.2.2:8100.
          </Text>

          <Text style={styles.label}>Username (optional)</Text>
          <TextInput
            style={styles.input}
            value={authUser}
            onChangeText={setAuthUser}
            autoCapitalize="none"
            autoCorrect={false}
            placeholderTextColor="#5b6b7d"
          />
          <Text style={styles.label}>Password (optional)</Text>
          <TextInput
            style={styles.input}
            value={authPass}
            onChangeText={setAuthPass}
            secureTextEntry
            autoCapitalize="none"
            autoCorrect={false}
            placeholderTextColor="#5b6b7d"
          />

          <View style={styles.buttons}>
            <Pressable style={[styles.btn, styles.btnCancel]} onPress={onClose}>
              <Text style={{ color: '#e6edf3' }}>Cancel</Text>
            </Pressable>
            <Pressable
              style={[styles.btn, valid ? styles.btnOk : styles.btnDisabled]}
              onPress={() =>
                valid &&
                onSave({
                  ...settings,
                  serverUrl: serverUrl.trim(),
                  authUser: authUser.trim(),
                  authPass,
                })
              }
            >
              <Text style={{ color: '#0b0f14', fontWeight: '600' }}>Save</Text>
            </Pressable>
          </View>
        </View>
      </Pressable>
    </Modal>
  );
}

const styles = StyleSheet.create({
  backdrop: {
    flex: 1,
    backgroundColor: 'rgba(0,0,0,0.6)',
    justifyContent: 'flex-end',
  },
  sheet: {
    backgroundColor: '#121821',
    borderTopLeftRadius: 16,
    borderTopRightRadius: 16,
    borderWidth: 1,
    borderColor: '#223041',
    padding: 16,
    paddingBottom: 24,
  },
  title: {
    color: '#e6edf3',
    fontSize: 17,
    fontWeight: '600',
    marginBottom: 12,
  },
  label: {
    color: '#8b98a9',
    fontSize: 12,
    marginTop: 10,
    marginBottom: 4,
  },
  hint: {
    color: '#8b98a9',
    fontSize: 11,
    marginTop: 4,
  },
  input: {
    backgroundColor: '#171f2a',
    borderColor: '#223041',
    borderWidth: 1,
    borderRadius: 8,
    color: '#e6edf3',
    fontSize: 14,
    paddingHorizontal: 10,
    paddingVertical: 8,
  },
  buttons: {
    flexDirection: 'row',
    justifyContent: 'flex-end',
    gap: 10,
    marginTop: 18,
  },
  btn: {
    borderRadius: 8,
    paddingHorizontal: 16,
    paddingVertical: 9,
  },
  btnCancel: {
    backgroundColor: '#171f2a',
    borderWidth: 1,
    borderColor: '#223041',
  },
  btnOk: {
    backgroundColor: '#3fb6ff',
  },
  btnDisabled: {
    backgroundColor: '#223041',
  },
});
