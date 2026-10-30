import { registerLocaleData } from '@angular/common';
import { loadTranslations } from '@angular/localize';

/** Languages of the console. English is the source language and the default. */
export const LANGUAGES = [
  { code: 'en', name: 'English' },
  { code: 'fr', name: 'Français' },
  { code: 'es', name: 'Español' },
  { code: 'de', name: 'Deutsch' },
  { code: 'pt', name: 'Português' },
  { code: 'zh', name: '简体中文' },
  { code: 'ja', name: '日本語' },
  { code: 'en-x-pirate', name: 'English pirate' },
  { code: 'en-x-emoji', name: 'English emoji' },
] as const;

export type LanguageCode = (typeof LANGUAGES)[number]['code'];

export const DEFAULT_LANGUAGE: LanguageCode = 'en';
const STORAGE_KEY = 'hfproxy.language';

interface MessageFile {
  translations: Record<string, string>;
}

// Each language is a separate chunk, fetched only when it is selected.
const messages: Record<Exclude<LanguageCode, 'en'>, () => Promise<{ default: MessageFile }>> = {
  fr: () => import('../../locale/fr.json'),
  es: () => import('../../locale/es.json'),
  de: () => import('../../locale/de.json'),
  pt: () => import('../../locale/pt.json'),
  zh: () => import('../../locale/zh.json'),
  ja: () => import('../../locale/ja.json'),
  'en-x-pirate': () => import('../../locale/en-x-pirate.json'),
  'en-x-emoji': () => import('../../locale/en-x-emoji.json'),
};

const localeData: Record<Exclude<LanguageCode, 'en'>, () => Promise<{ default: unknown[] }>> = {
  fr: () => import('@angular/common/locales/fr'),
  es: () => import('@angular/common/locales/es'),
  de: () => import('@angular/common/locales/de'),
  pt: () => import('@angular/common/locales/pt'),
  zh: () => import('@angular/common/locales/zh'),
  ja: () => import('@angular/common/locales/ja'),
  'en-x-pirate': () => import('@angular/common/locales/en'),
  'en-x-emoji': () => import('@angular/common/locales/en'),
};

export function isLanguage(value: unknown): value is LanguageCode {
  return LANGUAGES.some((language) => language.code === value);
}

/** The language chosen on this browser, or English when nothing was chosen. */
export function storedLanguage(): LanguageCode {
  try {
    const value = window.localStorage.getItem(STORAGE_KEY);
    if (isLanguage(value)) return value;
  } catch {
    // Storage can be blocked; English is the safe default.
  }
  return DEFAULT_LANGUAGE;
}

/**
 * Loads the translations of the chosen language before the application starts,
 * so every message is translated from the first render. A language that cannot
 * be loaded falls back to English instead of blocking the console.
 */
export async function loadLanguage(): Promise<LanguageCode> {
  const code = storedLanguage();
  document.documentElement.lang = code;
  if (code === 'en') return code;
  try {
    const [file, data] = await Promise.all([messages[code](), localeData[code]()]);
    loadTranslations(file.default.translations);
    registerLocaleData(data.default);
    return code;
  } catch (error) {
    console.error(`Could not load the ${code} translations; using English.`, error);
    document.documentElement.lang = DEFAULT_LANGUAGE;
    return DEFAULT_LANGUAGE;
  }
}

/** Remembers the language and reloads, because translations are bound at startup. */
export function setLanguage(code: LanguageCode): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, code);
  } catch {
    // The choice then only lasts until the next visit.
  }
  window.location.reload();
}
