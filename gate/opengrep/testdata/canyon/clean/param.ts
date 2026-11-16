export const GET = async (req: Request) => {
  const id = new URL(req.url).searchParams.get('id');
  return Response.json(await db.query('SELECT * FROM t WHERE id = $1', [id]));
};
