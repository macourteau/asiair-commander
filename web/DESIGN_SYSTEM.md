# ASIAIR Commander — Design System

The bespoke visual + component system for the SPA. Built plain (no Tailwind /
Chakra / MUI); theming is CSS custom properties + a `data-theme` attribute.

This document is the contract for contributors: consume the
tokens and components as described here rather than re-styling from scratch, so
the app stays one coherent system.

---

## 1. Visual direction

A utilitarian observatory control panel that is also calm and status-forward.
Dark-first (an observatory runs at night), light fully supported. **Semantic
status color does the heavy lifting** — but never alone.

- **Type.** System sans (`--font-sans`) for all UI chrome; **monospace with
  tabular numerals** (`--font-mono`, applied via the `.mono` class) for every
  piece of machine data — byte sizes, timestamps, file paths, hosts, template
  previews, counts. Data reads as data.
- **Accent.** A single cool cyan (`--accent`, "CCD readout") for links, focus,
  active nav, and progress — sitting *over* the semantic status palette, not
  competing with it.
- **Signature element.** The "safe to power off" hero band on each dashboard rig
  card: a full-width, tone-colored band (icon + large label) that dominates the
  card. It is the one loud element; everything else is quiet, dense data.

---

## 2. Theme tokens

All tokens live in `src/design/tokens.css`. Dark is defined on
`:root` / `:root[data-theme="dark"]`; light overrides on
`:root[data-theme="light"]`. The non-color scale tokens (spacing, type, radii,
motion, layout, z-index) are theme-independent on `:root`.

### How theming works

- `index.html` runs a tiny pre-paint script that sets `data-theme` from
  `localStorage["aac-theme"]` → `prefers-color-scheme` → `dark`, so there is no
  wrong-theme flash.
- `ThemeProvider` (`useTheme`) owns the runtime state, persists the choice, keeps
  `<html data-theme>` in sync, and follows the OS setting until the user makes an
  explicit choice.
- **Never hard-code a color.** Reference a token so both themes and future
  retuning work automatically.

### Color token families

| Group | Tokens | Use |
|---|---|---|
| Surfaces | `--bg`, `--surface`, `--surface-2`, `--surface-inset`, `--border`, `--border-strong` | Page → card → elevated → input backgrounds; hairline vs. strong borders |
| Text | `--text`, `--text-muted`, `--text-faint` | Primary / secondary / tertiary text |
| Accent | `--accent`, `--accent-hover`, `--accent-quiet`, `--on-accent` | Links, focus, active nav, primary button, progress |
| Status: safe (green) | `--safe-fg`, `--safe-solid`, `--safe-tint`, `--safe-border` | "safe to power off", verified, success |
| Status: working (amber) | `--work-fg`, `--work-solid`, `--work-tint`, `--work-border` | In progress, warnings |
| Status: problem (red) | `--problem-fg`, `--problem-solid`, `--problem-tint`, `--problem-border` | Errors, criticals, destructive |
| Status: neutral | `--neutral-fg`, `--neutral-solid`, `--neutral-tint`, `--neutral-border` | Unknown / offline / idle — **deliberately not green** |
| Status: info | `--info-fg`, `--info-tint`, `--info-border` | Informational callouts |
| Feedback | `--focus-ring`, `--overlay-scrim`, `--shadow-1/2/overlay` | Focus, modal scrim, elevation |

Each status family has four roles: **fg** (readable text/icon on a surface),
**tint** (translucent fill behind it), **border**, and **solid** (a saturated bar
or dot). This split is what lets a status render as color + icon + text and still
meet AA in both themes: the visible text is always the `fg` hue on a subtle
`tint`, never white-on-saturated.

**Consuming a tone generically:** the `.tone-safe` / `.tone-working` /
`.tone-problem` / `.tone-neutral` / `.tone-info` classes remap a status family
onto generic `--tone-fg` / `--tone-tint` / `--tone-border` / `--tone-solid`
variables. Put a `.tone-*` class on a container and style with `--tone-*` inside;
`StatusPill` and the dashboard hero band both work this way. Get the class from
`toneClass(tone)`.

### Scale tokens

- **Spacing** (4px grid): `--space-05` (2px) … `--space-16` (64px).
- **Type sizes:** `--text-xs` (12) → `--text-2xl` (40); line-heights
  `--leading-tight/snug/normal`; weights `--weight-regular/medium/semibold/bold`.
- **Radii:** `--radius-sm/md/lg/pill`.
- **Motion:** `--dur-fast/base/slow`, `--ease`. All motion is non-essential and
  suppressed under `prefers-reduced-motion`.
- **Layout:** `--nav-width`, `--topbar-height`, `--content-max`, `--touch-min`
  (44px).
- **z-index:** `--z-nav/sticky/toast/overlay`.

### Status → presentation mapping

`src/design/status.ts` is the single place that turns backend enums into
`{ tone, icon, label, help }`. Always render status through these so the
color+icon+text rule holds:

- `safeToPowerOffView(safe_to_power_off)` — renders exactly as computed; `unknown`
  maps to **neutral**, never green.
- `rigStateView(state)`
- `severityView(severity)`

---

## 3. Components

Import everything from the barrel: `import { … } from "@/design"`. Stylesheets
are loaded once in `main.tsx`; **components never import CSS**, and neither should
surfaces (except co-located surface CSS like `dashboard.css`).

Every component renders semantic HTML, is keyboard-operable, and shows a visible
focus ring. Interactive elements meet the 44px touch target on coarse pointers.

| Component | Purpose | Key props | Snippet |
|---|---|---|---|
| `Button` | Primary action (`<button>`) | `variant` (primary/secondary/ghost/danger), `size`, `block`, `loading`, `icon`, `iconAfter` | `<Button variant="primary" icon={<Icon name="scan" />}>Scan</Button>` |
| `IconButton` | Icon-only control | **`label` (required → aria-label)**, `icon`, `active` | `<IconButton label="Dismiss" icon={<Icon name="close" />} onClick={x} />` |
| `Card` | Surface container | `as` (default `section`), `pad`, `interactive` | `<Card as="article" pad>…</Card>` |
| `StatusPill` | Status as color+icon+text | `view` (a `StatusView`) **or** `tone`+`icon`+`label`; `size` | `<StatusPill view={rigStateView(rig.state)} />` |
| `Badge` | Count / label chip | `variant` (neutral/problem/accent); pass `aria-label` for live counts | `<Badge variant="problem" aria-label="3 alerts">3</Badge>` |
| `Field` | Labelled form wrapper | `label`, `help`, `example`, `error`, `required`; render-prop gives `{id, aria-describedby, aria-invalid}` | see below |
| `TextInput` | Styled input (fwd ref) | `invalid`, `mono`, native input props | `<TextInput {...f} mono value={v} onChange={…} />` |
| `Select` | Native select (fwd ref) | `invalid`, `options` or `<option>` children | `<Select options={opts} value={v} onChange={…} />` |
| `Toggle` | On/off switch (checkbox, `role=switch`) | `checked`, `onChange`, `label`, `disabled` | `<Toggle checked={x} onChange={setX} label="Managed" />` |
| `Dialog` | Focus-trapped modal (portal) | `open`, `onClose`, `title`, `footer`, `size` | `<Dialog open={o} onClose={c} title="Remove rig?">…</Dialog>` |
| `Table` | Semantic responsive table | `caption`, `columns`, `rows`, `getKey`, `empty` | `<Table caption="Frames" columns={cols} rows={rows} getKey={r=>r.id} />` |
| `Toast` | Transient result notification | `ToastProvider` at root; `useToast()` → `success/error/info/show` | `useToast().success("Rig paused.")` |
| `Spinner` | Busy indicator | `size`, `label` (also its a11y name) | `<Spinner label="Loading…" />` |
| `EmptyState` | Invitation to act | `icon`, `title`, `description`, `action` | `<EmptyState icon="devices" title="No rigs yet" …/>` |
| `Icon` | Inline SVG glyph | `name` (`IconName`), `size`, `title` (makes it non-decorative) | `<Icon name="alert" size={18} />` |

**Field pattern** (enforces the copy rules — help + concrete example +
actionable error, all wired via `aria-describedby`):

```tsx
<Field
  label="Target name"
  help="How target folders are named."
  example="'NGC 7000' becomes 'NGC7000'"
  error={err}
>
  {(f) => <TextInput {...f} value={v} onChange={(e) => setV(e.target.value)} />}
</Field>
```

**Icon set** (`IconName`): status `safe` / `working` / `problem` / `unknown` /
`info`; domain `rig` / `share` / `nas`; nav `dashboard` / `devices` / `settings`
/ `logs` / `alert`; actions `play` / `pause` / `run-now` / `reclassify` / `scan`
/ `add` / `trash` / `edit` / `external` / `update`; chrome `menu` / `close` /
`chevron-down` / `chevron-right` / `sun` / `moon` / `logout` / `clock`. Icons are
decorative (`aria-hidden`) unless given a `title`.

---

## 4. Accessibility conventions (requirements, not polish)

- **Never color alone.** Every status is color **+ icon + text**. Enforced by
  routing all statuses through `status.ts` and `StatusPill`. The active nav item
  also carries `aria-current`, not just the accent color.
- **Contrast.** Status `fg` hues are AA-tuned against their surface in both
  themes (Primer-derived values); body/muted text pass AA on `--bg`/`--surface`.
- **Keyboard.** Real `<button>`/`<nav>`/`<main>`/`<table>`; logical tab order;
  visible `:focus-visible` ring everywhere; `Dialog` traps focus, closes on Esc,
  and restores focus to the opener; the mobile drawer closes on Esc; a
  "Skip to main content" link is the first tab stop.
- **ARIA.** Icon-only controls require a `label`. Form fields associate a
  `<label>` and wire help/example/error via `aria-describedby`; errors use
  `role="alert"`.
- **aria-live.** `AnnouncerProvider` exposes `useAnnounce()` — a single polite,
  **coalescing** live region (a burst of SSE events collapses to one
  announcement). The SSE layer announces significant changes only (rig
  offline/online, alert raised), never routine frame chatter. Toasts announce via
  their own live region (assertive for errors).
- **Reduced motion.** `prefers-reduced-motion` zeroes transitions/animations
  globally and stops the spinner; no meaning is ever conveyed by motion alone.
- **Touch.** 44px minimum interactive targets on coarse pointers (`@media
  (pointer: coarse)`), plus a 44px `--touch-min` token.

---

## 5. Dual-density responsive approach

One component set, two layouts, via CSS — never a separate mobile tree.

- **App shell:** persistent sidebar at ≥960px; a sticky top bar + slide-in drawer
  below. The same `<Nav>` serves both; CSS decides docked vs. off-canvas.
- **Dashboard:** single-column stacked cards on mobile; a
  `repeat(auto-fill, minmax(330px, 1fr))` grid on desktop.
- **Glanceable vs. dense:** each rig card shows the glance essentials always
  (name, state, the safe-to-power-off hero, drain, active alerts, frames
  tonight). Secondary detail (current-file progress, per-share free space,
  timestamps, errors) sits in `.rigcard__detail`, which is **collapsed behind an
  expander on mobile and shown unconditionally on desktop** (the expander is
  `display:none` at ≥680px). The expander uses `aria-expanded`/`aria-controls`.
- Wide content (tables) scrolls inside its own container; the page body never
  scrolls horizontally.

---

## 6. Import conventions

- Path alias **`@` → `src/`** (configured in `tsconfig.json` + `vite.config.ts`).
- **Design system:** `import { Card, Button, StatusPill, Icon, useToast } from "@/design"`
- **Tokens helpers:** `toneClass`, `safeToPowerOffView`, `rigStateView`,
  `severityView` also come from `@/design`.
- **API + types + SSE:** `import { api, ApiError, useSnapshot, type RigStatus } from "@/api"`
- **Formatting:** `import { formatBytes, relativeTime, absoluteTime } from "@/util/format"` (or relative).
- **Stylesheets:** loaded once in `main.tsx` (`tokens.css` → `base.css` →
  `components.css`, then `shell.css`, `auth.css`). Surface-specific CSS is
  co-located and imported by the surface (see `Dashboard.tsx` → `dashboard.css`).
  Do not import design-system CSS from a surface.

### For contributors

- Build each new surface with these components; reach for a raw element only when
  nothing fits, and if you find yourself restyling a primitive, add a variant to
  the component instead.
- Add new endpoints to `src/api/client.ts` only — it is the single place any
  `/api` path may be constructed. Add matching wire types to `src/api/types.ts`.
- New icons go in `src/design/icons.tsx` (`ICONS` map + `IconName` union).
- New status meanings go through `status.ts`; do not invent ad-hoc color usage.
- Use the utility layer (`.stack`, `.cluster`, `.cluster-between`, `.grow`,
  `.truncate`, `.mono`, `.sr-only`) before writing new layout CSS.
