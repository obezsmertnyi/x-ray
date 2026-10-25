import nextConfig from "eslint-config-next"

export default [
  ...nextConfig,
  {
    rules: {
      // React compiler rules fire false positives on intentional ref/setState
      // patterns inside useEffect callbacks and SSE handlers.
      "react-hooks/refs": "warn",
      "react-hooks/set-state-in-effect": "warn",
      "react-hooks/purity": "warn",
    },
  },
]
