import React, { useCallback, useEffect, useState } from 'react';
import { StyleSheet, View } from 'react-native';
import { StatusBar } from 'expo-status-bar';
import Banner from './src/Banner';
import ChatScreen from './src/ChatScreen';
import SettingsSheet from './src/SettingsSheet';
import * as Storage from './src/storage';
import { ping } from './src/api';

export default function App() {
  const [settings, setSettings] = useState(null); // null until storage loads
  const [showSettings, setShowSettings] = useState(false);
  const [online, setOnline] = useState(null); // null = checking

  useEffect(() => {
    Storage.load().then(setSettings);
  }, []);

  // LAN/VPN liveness: probe /health when settings change, then every 15 s,
  // plus the web browser's own online/offline events.
  useEffect(() => {
    if (!settings) return undefined;
    let alive = true;
    const check = async () => {
      const ok = await ping(settings);
      if (alive) setOnline(ok);
    };
    check();
    const iv = setInterval(check, 15000);
    const goOnline = () => check();
    const goOffline = () => {
      if (alive) setOnline(false);
    };
    if (typeof window !== 'undefined' && window.addEventListener) {
      window.addEventListener('online', goOnline);
      window.addEventListener('offline', goOffline);
    }
    return () => {
      alive = false;
      clearInterval(iv);
      if (typeof window !== 'undefined' && window.removeEventListener) {
        window.removeEventListener('online', goOnline);
        window.removeEventListener('offline', goOffline);
      }
    };
  }, [settings]);

  const saveSettings = useCallback(async (s) => {
    setSettings(s);
    setShowSettings(false);
    await Storage.save(s);
  }, []);

  if (!settings) return <View style={styles.root} />;

  return (
    <View style={styles.root}>
      <StatusBar style="light" />
      <Banner online={online} serverUrl={settings.serverUrl} />
      <ChatScreen
        settings={settings}
        onOpenSettings={() => setShowSettings(true)}
        onModelSelect={(id) => saveSettings({ ...settings, model: id })}
      />
      {showSettings ? (
        <SettingsSheet
          settings={settings}
          onClose={() => setShowSettings(false)}
          onSave={saveSettings}
        />
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  root: {
    flex: 1,
    backgroundColor: '#0b0f14',
  },
});
