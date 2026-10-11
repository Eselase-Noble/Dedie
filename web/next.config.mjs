// The Go binary embeds and serves the static export, so the production build
// must not depend on a Node.js runtime. During `next dev` the API is proxied
// to a locally running server instead.
const dev = process.env.NODE_ENV === "development";

/** @type {import('next').NextConfig} */
const config = {
  trailingSlash: true,
  images: { unoptimized: true },
  reactStrictMode: true,
  ...(dev
    ? {
        async rewrites() {
          return [{ source: "/api/:path*", destination: "http://127.0.0.1:8080/api/:path*" }];
        },
      }
    : { output: "export", distDir: ".next" }),
};

export default config;
