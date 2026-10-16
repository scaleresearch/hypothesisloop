// The browser e2e run serves its own copy of the UI beside a running one, built into its own directory.
// The content security policy and HSTS depend on the request, so the middleware sets them.
const nextConfig = {
  distDir: process.env.NEXT_DIST_DIR ?? '.next',
  poweredByHeader: false,
  async headers() {
    return [{
      source: '/:path*',
      headers: [
        { key: 'X-Content-Type-Options', value: 'nosniff' },
        { key: 'Referrer-Policy', value: 'strict-origin-when-cross-origin' },
      ],
    }]
  },
}

export default nextConfig
