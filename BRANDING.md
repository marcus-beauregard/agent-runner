# Brand

The mark, the type, and the rules for using them. Everything here was decided against the thing itself, not picked from a mood board.

## The mark

A rectangular loop with one gap, and a dot resting in the gap.

That is the product: a closed, deterministic path with exactly one place a decision can enter. Not an arrow, not a spark, not a robot. The loop is the fixed pipeline; the gap is where the world gets in; the dot is the one thing that moved.

| Asset | What it is |
|---|---|
| `assets/logo.png` | 1024×1024, the mark, flat |
| `assets/logo-512.png` | same mark at 512, for avatars and org icons |
| `assets/logo-alternate-sheet.png` | first generation, a presentation sheet with variants — reference only, never publish it |

**Measured, not assumed** (from `logo.png`):

| Property | Value |
|---|---|
| Frame | 1024×1024, square |
| Ground | `#191919` — uniform to the corners, no panel or mockup |
| Ink | pure white, 4.4% of the frame |
| Mark bounds | 47% of the width, 32% of the height |
| Centring | dx +5px, dy 0 |
| Symmetry | ink splits 51 / 49 left to right |
| At 32px | 82 of 1024 pixels carry ink — it survives the smallest size we care about |

## Clear space and size

- **Clear space:** the height of the dot on all four sides. Nothing enters it — no text, no border, no screenshot edge.
- **Minimum size:** 32px. Below that the gap closes up and the mark becomes a rectangle.
- **Alignment:** the mark is centred in its own frame. Do not re-crop it tighter; the margin is part of the design.

## Colour

Monochrome. There is no accent colour, and that is deliberate: state belongs in words — `gate failed`, `refused`, `budget` — not in a colour nobody can read in a terminal.

| Token | Value | Use |
|---|---|---|
| Ground | `#191919` | the mark's own background; also the dark surface for any locked-up image |
| Ink | `#FFFFFF` | the mark, and type on the ground |
| Muted ink | `#A8A8A8` | captions and secondary text on the ground |

On a white page, put the mark on a `#191919` tile rather than recolouring the mark. Inverting it (white ground, charcoal mark) is allowed for print, but it's a second asset — generate it, don't filter it.

## Typography

Recommended by Gemini from the mark itself — it read the mark as a heavy uniform band, a tall x-height companion, and one clean break, and chose type that shares that construction rather than decorating it.

### Wordmark — JetBrains Mono, 500–600

Tall x-height, even stroke rhythm, clean terminals; a monospace reads as "tool", which is what this is. Set at **500–600**, never 700+: the mark's band already carries the weight, and a bolder wordmark makes the two compete. All lowercase, no letterspacing, one space between mark and word.

```css
font-family: "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Monaco,
             Consolas, "Liberation Mono", "Courier New", monospace;
```

### Docs body — IBM Plex Sans

Engineered proportions, steady x-height, restrained humanist curves. Reads as documentation, not marketing.

### UI and labels — Inter

Neutral, open counters, drawn for small sizes — the same job the mark does at 32px.

```css
font-family: "IBM Plex Sans", "Inter", -apple-system, BlinkMacSystemFont,
             "Segoe UI", Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
```

All three are OFL and self-hostable. Fallbacks matter because a README renders on machines with nothing installed, and the stack above degrades to the platform's own mono and UI faces without changing the layout.

### Weight against the mark

The wordmark sits **slightly lighter** than the mark's stroke, or optically matched — never heavier. The mark is quiet by construction; type that shouts next to it is the fastest way to make the pair look assembled rather than designed.

### Avoid

- Display and high-personality faces: they add a voice the mark does not have.
- Hairline or black weights: hairlines break in dark-mode READMEs, black crowds the negative space.
- Script, calligraphic or flared organic faces: the mark's softness comes from uniform caps and clean corners, not modulation.
- Widely-tracked all-caps as a "technical" gesture. It is a costume.

## Voice

The brand is the prose as much as the mark. Short, specific, unexcited.

- Say the thing. "The gate failed" before "an issue was encountered".
- Numbers over adjectives. `457 lines` beats `extensive changes`.
- Never ask for attention. The work is the argument.
- Dry humour is allowed. Enthusiasm is not.

## Usage

| Where | How |
|---|---|
| README header | mark at 96px, then the wordmark beside it, vertically centred. One row, no tagline underneath |
| Favicon | 32px and 64px from `logo-512.png`; check the gap is still visible at 32 |
| Social preview | mark centred on `#191919` at 1280×640, nothing else |
| Terminal banner | don't. The CLI prints its version and usage, not ASCII art |
| Docs | no mark on every page. Once at the top of the README is the whole budget |

## Don't

- No gradient, glow, bevel or drop shadow. The mark is flat because the tool is.
- No rotation, no perspective, no "3D".
- No recolouring the mark to match a theme; put it on a tile instead.
- No lockup with a second symbol, badge, or icon set.
- No sticker or mascot version. There is no character; there is a pipeline.
- Don't set the wordmark in caps, or with letterspacing, to look more technical.

## Regenerating

The mark came out of Gemini from a written brief — a rectangular loop, one gap, a dot in it, flat white on flat dark, legible at 32px, nothing else in frame. If it is ever regenerated, hold it to the measurements at the top of this file: square, uniform corners, mark inside 50% of the frame, centred, and the gap still visible at 32px. Those are the tests an image model fails silently.

---

*Mark generated 2026-11-21. Typography recommended against the image, then checked against what ships in a browser and a terminal.*
