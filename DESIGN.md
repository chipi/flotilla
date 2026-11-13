---
name: flotilla
description: The operator's control room for a fleet of agents that propose and act on real systems.
colors:
  signal-blue: "#1F4FD1"
  signal-blue-deep: "#163A9C"
  signal-blue-wash: "#EEF3FF"
  ink: "#17191E"
  slate: "#3D414A"
  muted: "#5B606B"
  paper: "#F6F6F3"
  panel: "#FFFFFF"
  sunken: "#FAFAF7"
  well: "#F4F4F0"
  chip-neutral: "#F0F0EB"
  hairline: "#E3E3DE"
  divider: "#EFEFEA"
  edge: "#C9CAC3"
  control-edge: "#7C808A"
  ok-ink: "#14532D"
  ok-wash: "#DDF1E5"
  ok-lamp: "#3DBE7A"
  wait-ink: "#8A3205"
  wait-wash: "#FCE8D8"
  wait-edge: "#C2410C"
  wait-band: "#FFF8F2"
  stop-ink: "#8F1D14"
  stop-wash: "#FBE4E2"
  stop-red: "#B42318"
  stop-band: "#FDF0EF"
  badge-orange: "#E8590C"
  night-edge: "#3A3E47"
  night-text: "#D7D9DE"
  night-muted: "#9A9EA8"
typography:
  headline:
    fontFamily: "Inter, system-ui, sans-serif"
    fontSize: "28px"
    fontWeight: 700
    lineHeight: 1.2
    letterSpacing: "-0.02em"
  title:
    fontFamily: "Inter, system-ui, sans-serif"
    fontSize: "18px"
    fontWeight: 600
    lineHeight: 1.3
  body:
    fontFamily: "Inter, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "Inter, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 600
    letterSpacing: "0.04em"
  exact:
    fontFamily: "'JetBrains Mono', monospace"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.5
rounded:
  chip: "5px"
  sm: "6px"
  control: "8px"
  card: "10px"
  panel: "12px"
  pill: "22px"
spacing:
  xxs: "4px"
  xs: "6px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "20px"
  xxl: "24px"
components:
  button-primary:
    backgroundColor: "{colors.signal-blue}"
    textColor: "{colors.panel}"
    rounded: "{rounded.card}"
    padding: "0 18px"
    height: "48px"
  button-primary-hover:
    backgroundColor: "{colors.signal-blue-deep}"
  button-secondary:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "0 16px"
    height: "44px"
  button-stop:
    backgroundColor: "{colors.stop-red}"
    textColor: "{colors.panel}"
    rounded: "{rounded.control}"
    padding: "0 16px"
    height: "44px"
  chip-wait:
    backgroundColor: "{colors.wait-wash}"
    textColor: "{colors.wait-ink}"
    rounded: "{rounded.chip}"
    padding: "3px 8px"
    typography: "{typography.label}"
  chip-ok:
    backgroundColor: "{colors.ok-wash}"
    textColor: "{colors.ok-ink}"
    rounded: "{rounded.chip}"
    padding: "3px 8px"
  chip-stop:
    backgroundColor: "{colors.stop-wash}"
    textColor: "{colors.stop-ink}"
    rounded: "{rounded.chip}"
    padding: "3px 8px"
  card:
    backgroundColor: "{colors.panel}"
    rounded: "{rounded.panel}"
    padding: "16px 18px"
  input:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "10px 12px"
  nav:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.night-text}"
    height: "56px"
  nav-tab-active:
    backgroundColor: "{colors.paper}"
    textColor: "{colors.ink}"
    rounded: "{rounded.sm}"
---

<!-- markdownlint-disable-next-line MD025 -->
# Design System: flotilla

## Overview

<!-- markdownlint-disable-next-line MD036 -->
**Creative North Star: "The Control Room"**

A quiet room with good instruments. The fleet works in the background and proposes; the operator
decides. Most of the time nothing needs a person, and the screen says so plainly. When something
does need a person, exactly one thing lights up: the decision, with everything needed to make it
in view. Calm is the resting state, and colour is spent only on state and on the one action.

The density is that of a well-kept instrument panel, not a dashboard wall. Text is short and
literal, numbers are exact, and anything that must match the recorded truth (a plan hash, a
command, a case id, a run id) is set in a monospace face so it reads as data, not prose. The
system is flat: depth comes from hairline borders and pale tints, never from shadows. It is
built for a phone first and a laptop second. Both get the same structure, and nothing scrolls
sideways.

**Key Characteristics:**

- Warm paper ground, white panels, near-black ink; one blue for the action and links.
- Status in three paired voices (ok, waiting, stopped): a dark ink on its own pale wash.
- Exact things in JetBrains Mono; everything else in Inter.
- Flat surfaces, 1px borders, 44px tap targets everywhere.
- One badge in the whole UI: how many things wait for the operator.

## Colors

A neutral, slightly warm palette with one blue and three status voices; colour means something
every time it appears.

### Primary

- **Signal Blue** (`signal-blue`): the primary action (Approve plan), links, focus rings, and
  progress that is on track. Its deeper shade is the hover and pressed state; its wash marks a
  selected or hovered row.

### Neutral

- **Ink** (`ink`): all primary text, and the background of the navigation (the night chrome).
- **Slate** (`slate`): secondary text that must still read well: labels in the decision box,
  helper lines.
- **Muted** (`muted`): metadata, timestamps, table headers. The lightest text colour allowed on
  paper or panel.
- **Paper** (`paper`): the page ground. **Panel** (`panel`): cards, tables, the decision box.
- **Sunken** and **Well** (`sunken`, `well`): zebra rows, the "what would run" code well.
- **Hairline**, **Divider**, **Edge** (`hairline`, `divider`, `edge`): card borders, row
  separators, and the stronger border of the decision box.
- **Control Edge** (`control-edge`): the border of every secondary button and text field, dark
  enough to meet 3:1 against white.
- **Night Edge**, **Night Text**, **Night Muted**: borders and text on the dark navigation.

### Status

- **OK** (`ok-ink` on `ok-wash`, lamp `ok-lamp`): verified, healthy, at auto.
- **Waiting** (`wait-ink` on `wait-wash`, edge `wait-edge`, band `wait-band`): waiting for
  the operator, propose, plan changed.
- **Stopped** (`stop-ink` on `stop-wash`, red `stop-red`, band `stop-band`): failed,
  escalated, rejected, frozen, irreversible.
- **Badge Orange** (`badge-orange`): only the count of things waiting for the operator.

**The Meaning Rule.** Colour is never decoration. Blue is the action, and the three status voices
are state; a coloured element that is neither is a bug.

**The Paired Voice Rule.** Status text is always its dark ink on its own wash (for example
`wait-ink` on `wait-wash`), never a coloured word on white or paper. Hue is never the only
signal: every status also carries its word ("waiting for you", "failed").

**The Red Means Stop Rule.** `stop-red` fills a surface only when autonomy is frozen, or on the
button that confirms a rejection. At rest the brakes are outlined, not red.

## Typography

**Body Font:** Inter (with system-ui, sans-serif)
**Exact Font:** JetBrains Mono (with monospace)

**Character:** a plain, legible sans for everything a person reads, and a monospace for
everything that must match the record byte for byte. The pairing separates "what we say" from
"what ran".

### Hierarchy

- **Headline** (700, 28px, 1.2, -0.02em; 24px on phones): the page title, one per page. Now's
  headline is a sentence ("7 cases handled overnight. 5 need you.").
- **Title** (600, 18px, 1.3): section heads and the decision's own title (balanced wrapping).
- **Body** (400, 14px, 1.5): running text, evidence, timelines; 15px inside status lines.
- **Label** (600, 12px, 0.04em, uppercase): table headers, section kickers ("Jump to a plan"),
  small field labels in the decision box (not uppercase there).
- **Exact** (400, 13px, 1.5, JetBrains Mono): hashes, commands, ids, routine names, run ids.

**The Exact Face Rule.** Anything the operator might copy, compare or type into a terminal is
set in the Exact face; nothing else is.

**The Sixteen Rule.** Text fields use 16px text, so phones never zoom on focus.

## Layout

The page is a single column of sections with a 1320px maximum width (880px for Approvals, where
one decision is read at a time), 24px page padding on a laptop and 16px on a phone. Sections
stack with 20–24px gaps; inside cards the rhythm is 8–14px. Two-column views (Now, Case) are a
wrapping flex row: content first (`flex: 999 1 560px`), the aside after (`flex: 1 1 340px`), so
on a phone the aside simply follows the content.

Grids that hold cards use `repeat(auto-fit, minmax(160px, 1fr))` or wider minimums, never a
fixed column count.

**The No Sideways Scroll Rule.** At 360–430px no page scrolls sideways. Tables become stacked
cards (each cell labelled with its column) below 640px; a wide diagram (the fleet workflow)
stacks vertically.

**The Thumb Rule.** Below 640px the four navigation groups move to a fixed bottom bar, and the
page reserves 104px at its foot so nothing hides under it.

## Elevation & Depth

Flat by design: there are no shadows. Depth comes from three things only: a 1px border
(`hairline`, or `edge` for the decision box), a pale tint behind a band or row (`sunken`,
`wait-band`), and the dark navigation above the light page.

**The Flat Rule.** No `box-shadow` on any surface. If something needs to stand out, give it a
stronger border or its status band, not a lift.

## Shapes

Gently rounded rectangles. Chips are 5px, small controls 6px, buttons and fields 8px, the
primary button and standard cards 10px, panels and the decision box 12px; filter pills and
sub-navigation tabs are fully rounded (22px). Borders are 1px; the only 2px border is the brakes
panel when autonomy is frozen.

## Components

### Buttons

- **Shape:** 8px corners; the primary 10px.
- **Primary** (Signal Blue, white text, 48px tall): one per decision, and it names what it does
  with its object ("Approve plan 4f1c…9e07"), never a bare "Approve".
- **Secondary** (white, `control-edge` border, ink text, 44px): Reject…, Copy the hash, Cancel.
- **Stop** (`stop-red`, white text): only the confirm of a rejection or an unfreeze, after a
  reason has been given; disabled until then (`chip-neutral` fill, `muted` text).
- **Focus:** a 3px Signal Blue outline with 2px offset on every interactive element.
- **Height:** every target is at least 44×44px; inline links get a padded hit area.

### Chips

- **Style:** status chips use the paired voices (label type, 5px corners); kind and target
  chips are neutral (`chip-neutral`) and act as filters on Cases.

### Cards / Containers

- **Corner Style:** 10–12px.
- **Background:** `panel` on `paper`.
- **Border:** 1px `hairline`; the decision box uses `edge`.
- **Internal Padding:** 16–18px.

### Inputs / Fields

- **Style:** white, 1px `control-edge`, 8px corners, 16px text, a visible label above.
- **Focus:** the Signal Blue focus ring.
- **Required reason:** the confirm button stays disabled until the field has text.

### Navigation

- **Top bar** (`ink`, 56px): brand, the four groups (Decide, Watch, Govern, Record) as tabs,
  the autonomy pill on the right. The active group is a `paper` tab with ink text.
- **Group pages:** a second row of rounded tabs for the pages inside the active group.
- **Phone:** the groups move to a bottom bar (icon + word, 56px targets); the badge rides on
  Decide.
- **The autonomy pill:** outlined at rest ("Autonomy: normal", green lamp); solid `stop-red`
  when frozen. It opens the brakes.

### The Decision Box (signature)

The one component everything else serves. A header band (status chip, case id, kind and fleet,
waiting time), the decision's title, then in this order: what would run (the action and the
literal host command, in a `well`), a grid of risk / if it goes wrong / approve from / proposed
by, why it asks you, the evidence, what happens after approval, the plan hash with its copy
button, the optional note, and the two answers.

- A changed plan shows the old plan until the operator opens the new one; then the box redraws
  from the new plan with a diff (added lines on `ok-wash`, removed on `stop-wash`, struck), and
  a "Changed at …" line stays until it is answered.
- An answer waits five seconds with Undo and Send now before it is sent.
- A failed send keeps the note and says nothing ran.
- A CLI-only plan shows the exact command with the full hash; on a phone it collapses to one
  line with the copy button.

## Do's and Don'ts

### Do

- **Do** name the object on every action button ("Approve plan 4f1c…9e07", "Reject plan …").
- **Do** set hashes, commands and ids in JetBrains Mono, and give the full value wherever it can
  be copied.
- **Do** pair every status colour with its word.
- **Do** check every page at 360, 390 and 1440px: no sideways scroll, 44px targets.
- **Do** keep loosening actions (unfreeze, promote) behind a required reason; tightening
  actions (freeze, demote) act at once and can be undone.

### Don't

- **Don't** use shadows, gradients or decorative colour.
- **Don't** put a second badge anywhere; the only count is what waits for the operator.
- **Don't** show red at rest: the brakes are quiet until used.
- **Don't** show an approve button for a plan whose current content is not on screen.
- **Don't** use fixed column counts for card grids, or tables that scroll sideways on a phone.
