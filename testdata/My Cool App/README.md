# My Cool App

A folder named the way people name folders. Nothing in here is a mistake — it
is simply not developer-shaped.

| What it looks like | What it tests |
|---|---|
| Folder `My Cool App` | The app name is derived from it. Spaces and capitals are not valid in a Kubernetes name or a hostname, so it has to become `my-cool-app` without anyone being asked to rename anything. |
| `styles/main style.css` | A space inside a filename. It has to survive being packed, uploaded, unpacked and served. |
| `images/IMG_2024 (1).svg` | Parentheses and a space — what every phone and every "duplicate file" produces. |
| No config of any kind | Static files, nothing else. |

## The test

`donkey deploy` in this folder should end at `https://my-cool-app.<space>.…`
with the stylesheet and the image both loading. A 404 on either one means a
filename was mangled somewhere between the laptop and the gateway.
