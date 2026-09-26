// The browser e2e run serves its own copy of the UI beside a running one, built into its own directory.
const nextConfig = {
  distDir: process.env.NEXT_DIST_DIR ?? '.next',
}

export default nextConfig
