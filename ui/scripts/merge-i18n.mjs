// Merges translations into the language files and drops the messages that no
// longer exist in src/locale/messages.json:
//   node scripts/merge-i18n.mjs translations.json
// where the file maps a language to {id: text}. New messages go at the end.
import { readFileSync, writeFileSync } from 'node:fs';

const root = new URL('../src/locale/', import.meta.url);
const read = (name) => JSON.parse(readFileSync(new URL(`${name}.json`, root), 'utf8'));
const source = read('messages').translations;
const added = JSON.parse(readFileSync(process.argv[2], 'utf8'));
for (const [language, texts] of Object.entries(added)) {
  const file = read(language);
  const kept = Object.entries(file.translations).filter(([id]) => id in source);
  file.translations = Object.fromEntries([...kept, ...Object.entries(texts)]);
  writeFileSync(new URL(`${language}.json`, root), JSON.stringify(file, null, 2) + '\n');
}
