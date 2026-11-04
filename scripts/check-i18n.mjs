#!/usr/bin/env node
// Fails when a UI string passed to t('...') (or a validation message written as
// key: '...') has no entry in the English locale file. Run: yarn check-i18n
// Add --fix to append the missing keys (value = key) to the locale file.
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const localeDir = 'locales/en';
const localeFile = join(localeDir, readdirSync(localeDir).find((f) => f.endsWith('.json')));
const locale = JSON.parse(readFileSync(localeFile, 'utf8'));

const files = [];
const walk = (dir) => {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) walk(path);
    else if (/\.(ts|tsx)$/.test(name)) files.push(path);
  }
};
walk('src');

// t('...') calls, plus validation messages written as key: '...'. A key: value
// without a space is an identifier (label selector, option id), not UI text.
const pattern = /(\bt\(|\bkey:)\s*'((?:[^'\\]|\\.)*)'/g;

/** The string value of a single-quoted JS literal body (\', \u2019, \n, ...). */
function unescape(raw) {
  try {
    return JSON.parse(`"${raw.replace(/\\'/g, "'").replace(/(^|[^\\])"/g, '$1\\"')}"`);
  } catch {
    return raw.replace(/\\'/g, "'");
  }
}

const missing = new Map();
for (const file of files) {
  const src = readFileSync(file, 'utf8');
  for (const m of src.matchAll(pattern)) {
    const key = unescape(m[2]);
    if (m[1] !== 't(' && !key.includes(' ')) continue;
    if (!(key in locale) && !missing.has(key)) missing.set(key, file);
  }
}

if (missing.size === 0) {
  console.log(`i18n: all keys present in ${localeFile}`);
  process.exit(0);
}
if (process.argv.includes('--fix')) {
  for (const key of missing.keys()) locale[key] = key;
  writeFileSync(localeFile, `${JSON.stringify(locale, null, 2)}\n`);
  console.log(`i18n: added ${missing.size} key(s) to ${localeFile}`);
  process.exit(0);
}
console.error(`i18n: ${missing.size} key(s) missing from ${localeFile} (run "yarn check-i18n --fix"):`);
for (const [key, file] of missing) console.error(`  ${file}: ${key}`);
process.exit(1);
