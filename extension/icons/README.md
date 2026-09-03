# Icons

One directory per state; the toolbar icon switches between them as the routing
state changes (see `iconState` in `background.js`). `brand` is the app icon.

- `icon48.png` and `icon128.png` are the designed tile: slate square, off-white
  "t", 8-unit margin, 8-unit tail dot.
- `icon16.png` and `icon32.png` are rendered from `small.svg`, a simplified
  drawing for toolbar sizes: the tile fills the canvas, the "t" is heavier and
  the dot is 20 units, so the state colour is visible at 16 px.

Re-render the small sizes with ImageMagick:

```sh
magick -background none -density 512 small.svg -resize 16x16 icon16.png
magick -background none -density 512 small.svg -resize 32x32 icon32.png
```
