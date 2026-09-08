# CLI Commands

The Construct CLI includes Graph commands for initializing models, generating scaffolds, and registering schemas.

## construct graph init

Add Graph SDK to your space:

```bash
construct graph init
```

This installs `@construct-space/graph` and creates a `models/` directory.

## construct graph g (generate)

Generate a model file with fields:

```bash
# Basic model
construct graph g Task title:string done:boolean priority:enum:low,medium,high

# Model with relations
construct graph g Post title:string body:string author:belongsTo:User

# Model with modifiers
construct graph g Employee name:string email:string:unique salary:number
```

### Field syntax

```
fieldName:type[:modifier|:target]
```

| Pattern | Example | Result |
|---------|---------|--------|
| `name:string` | `title:string` | `field.string()` |
| `name:int` | `age:int` | `field.int()` |
| `name:number` | `salary:number` | `field.number()` |
| `name:boolean` | `done:boolean` | `field.boolean()` |
| `name:date` | `started_at:date` | `field.date()` |
| `name:json` | `metadata:json` | `field.json()` |
| `name:enum:a,b,c` | `status:enum:draft,published` | `field.enum(['draft', 'published'])` |
| `name:string:unique` | `email:string:unique` | `field.string().unique()` |
| `name:belongsTo:Model` | `author:belongsTo:User` | `relation.belongsTo(User)` |
| `name:hasMany:Model` | `posts:hasMany:Post` | `relation.hasMany(Post)` |

## construct graph push

Register your models with Graph without publishing the space:

```bash
construct graph push
```

This sends the current models to the Graph service, which creates/updates the database schema and tables. Useful during development for testing schema changes.

## construct publish

Full space publish (includes Graph registration):

```bash
construct publish
```

When a space with models is published, the CLI:

1. Builds the space
2. Packs the source
3. Uploads to the developer portal
4. Developer portal registers the schema with Graph
5. Graph creates/migrates the database tables
