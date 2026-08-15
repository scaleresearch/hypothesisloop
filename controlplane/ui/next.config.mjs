const nextConfig = {
  env: {
    // The control plane serves its whole API from one origin; see src/lib/api.ts.
    NEXT_PUBLIC_API_URL: process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8081',
  },
}

export default nextConfig
