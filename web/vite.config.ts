import { defineConfig } from 'vite-plus'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://viteplus.dev/config/ — one file for the dev server and build, and
// for the linter (vp lint) and formatter (vp fmt) beside them.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // Local development against a port-forwarded donkey-server.
    proxy: { '/v1': 'http://localhost:8088' },
  },
  lint: {
    plugins: ['react', 'typescript', 'oxc'],
    rules: {
      'react/rules-of-hooks': 'error',
      'react/only-export-components': ['warn', { allowConstantExport: true }],
    },
    ignorePatterns: ['dist/**'],
  },
  // The console's own style: no semicolons, single quotes, long lines.
  // Imports, Tailwind classes and package.json keep the order they are
  // written in.
  fmt: {
    semi: false,
    singleQuote: true,
    printWidth: 120,
    trailingComma: 'all',
    arrowParens: 'always',
    sortImports: false,
    sortTailwindcss: false,
    sortPackageJson: false,
    ignorePatterns: ['dist/**', 'package-lock.json'],
  },
})
