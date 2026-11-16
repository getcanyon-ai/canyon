export async function GET(req: Request) {
  const q = new URL(req.url).searchParams.get("q");
  return Response.json(await pool.query({ text: "SELECT * FROM t WHERE q = $1", values: [q] }));
}
