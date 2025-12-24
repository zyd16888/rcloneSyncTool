/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./internal/server/templates/**/*.html"],
  theme: {
    extend: {},
  },
  plugins: [require("daisyui")],
  daisyui: {
    themes: ["winter", "dark"],
  },
};
