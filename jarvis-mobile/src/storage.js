// Settings persistence. AsyncStorage is the one store that works the same on
// iOS, Android and web (localStorage there), so server URL / credentials /
// selected model survive restarts everywhere.
import AsyncStorage from '@react-native-async-storage/async-storage';
import { DEFAULT_SERVER } from './api';

const K = {
  server: 'jarvis.serverUrl',
  user: 'jarvis.authUser',
  pass: 'jarvis.authPass',
  model: 'jarvis.model',
};

export async function load() {
  const [serverUrl, authUser, authPass, model] = await Promise.all([
    AsyncStorage.getItem(K.server),
    AsyncStorage.getItem(K.user),
    AsyncStorage.getItem(K.pass),
    AsyncStorage.getItem(K.model),
  ]);
  return {
    serverUrl: serverUrl || DEFAULT_SERVER,
    authUser: authUser || '',
    authPass: authPass || '',
    model: model || '', // '' = let the server pick its default model
  };
}

export async function save(s) {
  await Promise.all([
    AsyncStorage.setItem(K.server, s.serverUrl),
    AsyncStorage.setItem(K.user, s.authUser),
    AsyncStorage.setItem(K.pass, s.authPass),
    AsyncStorage.setItem(K.model, s.model || ''),
  ]);
}
