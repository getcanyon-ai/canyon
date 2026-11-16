export async function GET(req: Request, { params }: { params: { id: string } }) {
  const { name } = await req.json();
  await db.query(`INSERT INTO t (name) VALUES ('${name}')`);
  return Response.json({});
}
