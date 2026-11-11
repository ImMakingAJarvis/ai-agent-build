import React from 'react';
import { StyleSheet, Text, View } from 'react-native';

// Status strip at the top of the screen. The app is LAN/VPN-only by
// construction, so "offline" almost always means "not on the home network".
// online: null = checking, true = brain reachable, false = unreachable.
export default function Banner({ online, serverUrl }) {
  if (online === true) return null;
  return (
    <View style={styles.bar}>
      <Text style={styles.text} numberOfLines={2}>
        {online === null
          ? '⚙ Connecting to Jarvis…'
          : '⚠ Offline — connect to home WiFi or the VPN (' + serverUrl + ')'}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  bar: {
    backgroundColor: '#3a1d1d',
    paddingHorizontal: 12,
    paddingVertical: 8,
    borderBottomWidth: 1,
    borderBottomColor: '#7a3b3b',
  },
  text: {
    color: '#ffd7d5',
    fontSize: 13,
  },
});
