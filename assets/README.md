# Visual assets

scruff's mark is the family's paired cat-ears over the thing scruff actually
draws: three lanes side by side, the middle one lit and the two beside it
parked. A lane per agent. The standard tile is flat geometry in three
[nebelung](https://github.com/hausfold/nebelung) tokens: `maroon` (#E6A3AD) for
the ears and the live lane, `surface0` (#343434) for the tile, `surface1`
(#494949) for the two parked lanes. It reads at 16 px and sits next to perch's
`green` cards, trill's `yellow` card and pounce's `peach` input bar.

The light tile is the same drawing in nebelung's latte set. That ramp runs the
other way, so the tile is `base` (#F1F1F1) and the parked lanes `surface1`
(#C0C0C0) step darker than it instead of lighter; the ears and the live lane
are latte `maroon` (#DE5059). A light artifact is latte, and there is no light
*inverted* tile: an inverted one already carries its own colour.

The inverted tile turns that over. Maroon ground, `surface0` ears and live
lane, and the two parked lanes the same `surface0` at 0.45, because they sit on
the tile ground rather than on another shape and so dim to it rather than
stepping darker.

| file | what it is |
|---|---|
| `scruff-square.svg` | **The mark's source of record.** A 100-unit viewBox, colours as nebelung hexes; the brand kit's [`docs/design.md`](https://github.com/hausfold/workshop/blob/main/docs/design.md) is the standard it answers to. The PNG beside it renders from this file, at any size. |
| `scruff-square.png` | 2048×2048, rendered from it. |
| `scruff-square-inverted.svg` | **The inverted tile's source of record**, same geometry, same viewBox: maroon ground, dark shapes. For a light *page*, where latte would clash with the page's own palette, and for the logo sheet where the standard tile would disappear into it. The light tile is the one for a light *artifact*. |
| `scruff-square-inverted.png` | 2048×2048, rendered from it. |
| `scruff-square-latte.svg` | **The light tile's source of record**, same geometry, same viewBox, in nebelung's latte set. |
| `scruff-square-latte.png` | 2048×2048, rendered from it. |
| `scruff-banner.png` | 1200×348 identity banner, the maroon wordmark beside the mark on a rounded graphite tile, on the family's shared banner lockup. What the README opens with. |
| `scruff-banner-inverted.png` | 1200×348, the same lockup turned over: maroon ground, `crust` wordmark, and `A LANE PER AGENT` under it. The one place the tagline is drawn, and the banner for a light ground, where the graphite one would sit in a hole. |

scruff ships no app and so no app icon: there are no icon slots to derive, and
no iOS square. The two banners are raster only, the way every banner in the
family is, because the wordmark is type and this repo holds no source for it.

Those hexes are **baked into the PNGs**, and nothing here follows a theme. A
palette change in nebelung means swapping the hexes in all three SVGs,
re-rendering each PNG from its own source, and redrawing both banners from the
brand kit, which the SVGs cannot do for you:

```sh
# resvg is what the committed PNGs were checked against; a different rasteriser
# will not land byte-for-byte on them.
for f in scruff-square scruff-square-inverted scruff-square-latte; do
  nix run nixpkgs#resvg -- "assets/$f.svg" "assets/$f.png"
done
```

The ears are the family path, used verbatim. Move them, scale them, recolour
them; never redraw them. Every other rule these files answer to, and the
geometry written out as text, is the brand kit's `docs/design.md` under
*Components*; the index of every mark in the family is its
[`assets/README.md`](https://github.com/hausfold/workshop/blob/main/assets/README.md),
which `hausfold.co/brand` lands on.
