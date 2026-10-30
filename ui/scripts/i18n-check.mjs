// Checks the console translations:
//   1. every piece of user-visible text in a template is marked for translation;
//   2. every language file has exactly the messages of src/locale/messages.json
//      (run `npm run i18n:extract` first), with the same placeholders.
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';

const require = createRequire(import.meta.url);
const { parseTemplate } = await import(require.resolve('@angular/compiler'));
const root = new URL('../src/app', import.meta.url).pathname;
const problems = [];

function files(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) files(path, out);
    else if (path.endsWith('.html')) out.push(path);
  }
  return out;
}

function translatable(text) {
  const stripped = text
    .replace(/\{\{[\s\S]*?\}\}/g, ' ')
    .replace(/&[a-z]+;/g, ' ')
    .trim();
  if (!/\p{L}{2}/u.test(stripped)) return false;
  return !(/^[A-Z0-9_\-./:,\s]+$/.test(stripped) && !/\s[A-Za-z]/.test(stripped));
}

for (const file of files(root)) {
  const src = readFileSync(file, 'utf8');
  const { nodes, errors } = parseTemplate(src, file, { preserveWhitespaces: true });
  if (errors?.length) problems.push(`${file}: ${errors.map((e) => e.msg).join('; ')}`);
  const visit = (list, insideCode, marked = false) => {
    for (const node of list) {
      const kind = node.constructor.name;
      if (kind === 'Text' || kind === 'BoundText') {
        const text = src.slice(node.sourceSpan.start.offset, node.sourceSpan.end.offset);
        if (!insideCode && !marked && translatable(text)) {
          const line = src.slice(0, node.sourceSpan.start.offset).split('\n').length;
          problems.push(
            `${file}:${line}: text is not marked for translation: ${text.trim().slice(0, 60)}`,
          );
        }
        continue;
      }
      const code = insideCode || (kind === 'Element' && ['code', 'pre', 'kbd'].includes(node.name));
      const tag =
        kind === 'Element'
          ? src.slice(node.startSourceSpan.start.offset, node.startSourceSpan.end.offset)
          : '';
      const inside = marked || /\si18n(\s|=|>)/.test(tag);
      visit(node.children ?? [], code, inside);
      for (const key of ['branches', 'groups', 'cases']) {
        for (const child of node[key] ?? []) visit(child.children ?? [], code, inside);
      }
      if (node.empty) visit(node.empty.children ?? [], code, inside);
    }
  };
  visit(nodes, false);
}

const placeholders = (text) =>
  [...text.matchAll(/\{\$[A-Z0-9_]+\}|\{[A-Z_]+,\s*plural|\{[A-Z_]+\}/g)]
    .map((m) => m[0])
    .sort()
    .join(',');
const read = (name) =>
  JSON.parse(readFileSync(new URL(`../src/locale/${name}.json`, import.meta.url), 'utf8'));
const source = read('messages').translations;
const languages = ['fr', 'es', 'de', 'pt', 'zh', 'ja', 'en-x-pirate', 'en-x-emoji'];
for (const language of languages) {
  const translated = read(language).translations;
  for (const [id, text] of Object.entries(source)) {
    if (!(id in translated))
      problems.push(`${language}: missing translation of "${text.slice(0, 60)}" (${id})`);
    else if (!translated[id].trim()) problems.push(`${language}: empty translation (${id})`);
    else if (!text.includes('plural') && placeholders(text) !== placeholders(translated[id]))
      problems.push(`${language}: placeholders differ for "${text.slice(0, 60)}" (${id})`);
  }
  // The English variants may translate messages ahead of the branch that adds them.
  if (language.startsWith('en-x-')) continue;
  for (const id of Object.keys(translated))
    if (!(id in source)) problems.push(`${language}: unused translation ${id}`);
}

if (problems.length) {
  console.error(problems.join('\n'));
  console.error(`\n${problems.length} i18n problem(s)`);
  process.exit(1);
}
console.log(`i18n OK: ${Object.keys(source).length} messages in ${languages.length} languages`);
