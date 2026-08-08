# Digital Greenhouse

A living, interactive digital greenhouse: a single-page web app rendering a
procedurally generated greenhouse at night, with bioluminescent plants,
floating pollen, fireflies, and rain on the glass.

Built end to end by a self-hosted 27B model (Qwopus3.6-27B-Fusion) running on a
DGX Spark fleet, orchestrated by LLMKube Foreman, reviewed by Nemotron-49B on a
Strix Halo box.

## Running it

Open `index.html` directly in a browser. No build step, no server, no network
access required at runtime.

## Constraints

These are requirements, not preferences:

- **Single page.** `index.html` plus `vendor/` is the whole deliverable.
- **No CDNs.** Three.js is vendored in `vendor/`. The page must work fully
  offline, opened from `file://`.
- **No frameworks.** Vanilla JavaScript, no React/Vue/build tooling.
- **It must render.** A stage that produces a black canvas is not done,
  however good the code looks.

## Architecture

Modular classes, one responsibility each: `Greenhouse` (structure and glass),
`Plant` / `PlantSystem` (procedural growth), `ParticleSystem` (pollen,
fireflies, bursts), `Weather` (rain, wind, time of night), `UI` (stats panel),
`App` (bootstrap and the render loop).

Performance is a requirement, not a nicety: pooled particles, no allocation in
the render loop, 60fps on a laptop.
