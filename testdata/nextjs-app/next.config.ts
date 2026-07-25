import type { NextConfig } from 'next'

// `output: 'standalone'` is the line the generator leaves commented out and
// nobody uncomments. Without it a deploy has to keep all of node_modules to
// run `next start`. donkey should cope either way.
const nextConfig: NextConfig = {}

export default nextConfig
