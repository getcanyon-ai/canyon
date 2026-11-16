// A file imported as its text (Vite's ?raw): the reference set-up the dashboard shows filled in (deploy/gitops).
declare module "*?raw" {
  const text: string;
  export default text;
}
