export async function GET(req: Request, { params }: { params: { id: string } }) {
  const id = req.nextUrl.searchParams.get("id");
  return Response.json(await db.query("SELECT * FROM t WHERE id = " + id));
}
