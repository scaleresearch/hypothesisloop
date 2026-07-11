const nextConfig = {
  env: {
    API_BASE_URL: process.env.API_BASE_URL || 'http://localhost:8082',
    REGISTRY_URL: process.env.REGISTRY_URL || 'http://localhost:8083',
    QUOTA_URL: process.env.QUOTA_URL || 'http://localhost:8081',
  },
}

export default nextConfig
