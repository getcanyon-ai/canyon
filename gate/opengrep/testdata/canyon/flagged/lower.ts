export async function GET(req: Request, { params }: { params: { id: string } }) {
  return Response.json(await db.query(`
    select * from t where tenant = ${req.headers.get("x-tenant")}`));
}
