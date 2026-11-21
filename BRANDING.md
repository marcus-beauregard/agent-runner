# Brand

The mark, the type, the palette, and the rules for using them. Everything here was decided against the thing itself, not picked from a mood board.

## The mark

A rectangular loop with one gap on the right, and a dot resting in the gap.

That is the product: a closed, deterministic path with exactly one place a decision can enter. Not an arrow, not a spark, not a robot. The loop is the fixed pipeline; the gap is where the world gets in; the dot is the only thing that moved.

| Asset | What it is |
|---|---|
| `assets/logo.png` | 1024×1024 master, white mark on `#111111` |
| `assets/logo-512.png` | same mark at 512, for avatars and org icons |
| `assets/brand-poster.png` | the one-page guide: mark, clearance, type, colour, themes |

**Measured from the master, not assumed:**

| Property | Value |
|---|---|
| Frame | 1024×1024, square |
| Ground | `#111111` — identical in all four corners, no panel or mockup |
| Ink | pure white, 4.4% of the frame |
| Mark bounds | 47% of the width, 32% of the height |
| Centring | dx +5px, dy 0 |
| Symmetry | ink splits 51 / 49 left to right |
| Slit height | 28px at 1024, i.e. 2.7% of the frame — the number the minimum size is derived from |

## Clear space and size

- **Clear space:** the height of the dot on all four sides. Nothing enters it — no text, no border, no screenshot edge.
- **Minimum size: 64px.** The gap is two slits of 28px in a 1024 frame, so at any rendered width they are `0.027 × width` pixels: 0.9px at 32, 1.4px at 52, 1.75px at 64. Below roughly 56px they stop resolving and the mark reads as a plain rounded rectangle — which is a different logo. 64px is the first size where the thing the mark is *about* is still visible.
- **Alignment:** centred in its own frame. Do not re-crop tighter; the margin is part of the design.
- **Generous margins on every surface.** The mark earns attention by having room, not by being large.

## Colour

| Token | Value | Use |
|---|---|---|
| Ground | `#111111` | the mark's own background; the dark surface for any locked-up image |
| Ink | `#FFFFFF` | the mark, and type on the ground |

Monochrome by design. State is carried by words — `gate failed`, `refused`, `budget` — not by a hue nobody can read in a terminal. Invert (white ground, charcoal mark) only when a light surface is explicitly required, and generate that asset rather than filtering this one.

### Themes are previews, not the brand

The guide shows six standard theme presets in a selector row. They exist because a CLI user may already have one; they are **never** the canonical identity, and the mark does not get recoloured to match them.

| Theme | Base surface |
|---|---|
| Catppuccin | `#1E1E2E` (Mocha) |
| Solarized | `#002B36` dark / `#FDF6E3` light |
| Dracula | `#282A36` |
| Nord | `#2E3440` |
| Gruvbox | `#282828` |
| Tokyo Night | `#1A1B26` |

## Typography

| Role | Face | Stacks |
|---|---|---|
| Headings, section titles | **Iosevka Charon** | `"Iosevka Charon", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace` |
| Body, docs, technical notes | **Didact Gothic** | `"Didact Gothic", -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif` |
| Code, commands, flags, hex | **Oxygen Mono** | `"Oxygen Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace` |

- **Headings are the only place Iosevka Charon appears.** It sets hierarchy; it does not set prose.
- **Hex values are always Oxygen Mono**, so `#111111` reads as a literal and not as a word.
- **Wordmark weight sits slightly lighter than the mark's stroke** — the band already carries the mass.
- All three families are OFL. Two caveats, recorded rather than papered over: Didact Gothic and Oxygen Mono are on Google Fonts and self-host trivially; **Iosevka Charon is not a stock Iosevka variant** in upstream's variant definitions, so confirm the exact family name and build before adding a webfont. Until then the monospace stack degrades to the platform's own.

An earlier pass recommended JetBrains Mono / IBM Plex Sans / Inter. That trio is superseded by the system above, which was finalised against the mark and the poster.

## Voice

The brand is the prose as much as the mark. Short, specific, unexcited.

- Say the thing. "The gate failed" before "an issue was encountered".
- Numbers over adjectives. `457 lines` beats `extensive changes`.
- Never ask for attention. The work is the argument.
- Dry humour is allowed. Enthusiasm is not.

## Usage

| Where | How |
|---|---|
| README header | mark at 96px, wordmark beside it, vertically centred. One row, no tagline underneath |
| Favicon | 64px from `logo-512.png`. At 32px the gap is sub-pixel and the mark becomes a rounded rectangle — use 64, or accept the silhouette knowingly |
| Social preview | mark centred on `#111111` at 1280×640, nothing else |
| Docs | once at the top of the README is the whole budget. Not every page |
| Terminal banner | don't. The CLI prints its version and usage, not ASCII art |

## Don't

- No gradient, glow, bevel, shadow or texture. The mark is flat because the tool is.
- No rotation, perspective, or "3D".
- No recolouring the mark to match a theme or an OS accent.
- No lockup with a second symbol, badge, or icon set.
- No sticker, mascot, or character version. There is a pipeline, not a personality.
- No tracked all-caps wordmark as a "technical" gesture. It is a costume.

## Regenerating

The mark and the poster came out of Gemini from a written brief — a rectangular loop, one gap, a dot in it, flat white on flat dark, nothing else in frame. If either is regenerated, hold it to the measurements at the top of this file: square, four identical corners, mark inside 50% of the frame, centred, and a slit height of at least 2.7% so the gap survives to 64px. Those are the checks an image model fails silently — the first generation came back 1408×768 with four different corner colours and was thrown away.

One correction worth keeping visible: an earlier version of this file claimed a 32px minimum and offered ink coverage as the proof. Ink coverage does not test the gap — at 32px the mark was measured as 79 lit pixels *and* a plain rounded rectangle, because the slits were 0.875px. Counting ink is not the same test as seeing the hole, and it is the hole the rules are built on.

---

*Mark generated 2026-11-21. Design system and one-page guide finalised the same day; this document is the text form of that poster.*
