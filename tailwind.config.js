/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./internal/server/templates/**/*.html", "./internal/server/static/**/*.js"],
  theme: {
    extend: {},
  },
  plugins: [require("daisyui")],
  daisyui: {
    themes: ["winter", "dark"],
  },
};
