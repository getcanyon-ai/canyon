export default function Login() {
  return (
    <form action="/api/session" method="post">
      <h1>Sign in</h1>
      <label>Email <input name="email" type="email" required /></label>
      <label>Password <input name="password" type="password" required /></label>
      <button type="submit">Sign in</button>
    </form>
  );
}
