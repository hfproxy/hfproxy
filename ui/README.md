# hfproxy admin UI

Angular 22 administration console. Tailwind CSS 4 is processed by Angular through `.postcssrc.json`.

## Local development

Use the Node release pinned in `.nvmrc`, install the locked dependencies, then start the Angular dev server:

```sh
nvm use
npm ci
npm start
```

The dev server runs at `http://localhost:4200` and proxies API requests to the Go service on `http://127.0.0.1:8080`.

## Production build

```sh
npm run build
```

The production assets are written to `dist/hfproxy/browser` and embedded in the Go server image.

## Translating the console

The console is written in English and translated at run time with Angular's `$localize` and `@angular/localize`. English is the default; French (`fr`), Spanish (`es`), German (`de`), Portuguese (`pt`), Simplified Chinese (`zh`) and Japanese (`ja`) are translated, along with two English variants, pirate (`en-x-pirate`) and emoji (`en-x-emoji`), which keep the English date and number formats. The chosen language is stored in the browser (`localStorage`, key `hfproxy.language`) and applied at start-up, so changing it reloads the page. Each language is a separate lazy chunk, loaded only when selected.

- Mark text in a template with `i18n` (and attributes with `i18n-title`, `i18n-placeholder`, `i18n-aria-label`, and so on). In TypeScript use the tagged template ``$localize`Text ${value}:NAME:` ``. Do not build sentences by concatenation.
- After changing any text, run `npm run i18n:extract && npx prettier --write src/locale/messages.json`. It rewrites `src/locale/messages.json` (message id → English source). Add the new ids to every `src/locale/<code>.json`, keeping the `{$PLACEHOLDER}` tokens unchanged.
- `npm run i18n:check` fails when a template text is not marked for translation, when a language misses a message or has an unused one, or when placeholders differ. CI runs it, and also fails if `messages.json` is out of date.
- To add a language: create `src/locale/<code>.json` with the same shape (`{"locale": "<code>", "translations": {…}}`), add it to `LANGUAGES`, `messages` and `localeData` in `src/app/core/i18n.ts` (the locale data comes from `@angular/common/locales/<code>`), and to the list in `scripts/i18n-check.mjs`.

Not translated: error messages returned by the Go API, the server-rendered sign-in page at `/login`, and the audit log's action names, which are data.
