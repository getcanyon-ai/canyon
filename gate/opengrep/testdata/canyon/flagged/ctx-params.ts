export async function GET(req: Request, ctx: { params: { id: string } }) {
  return Response.json(await db.query(`SELECT * FROM t WHERE id = ${ctx.params.id}`));
}
