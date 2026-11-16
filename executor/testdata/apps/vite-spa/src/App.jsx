export default function App() {
  const page = window.location.pathname === "/reports" ? "Reports" : "Overview";
  return (
    <main>
      <h1>Team dashboard</h1>
      <p>{page}</p>
    </main>
  );
}
